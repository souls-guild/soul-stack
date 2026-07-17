# redis - unified Redis service (Ansible-role concept)

The "redis" service is **one service for all Redis deployment modes**, following the
concept of the Ansible Redis role. The mode is selected via the `redis_type` field
(`sentinel` / `cluster`), rather than a separate service per stack.

The operator specifies **simple typed concepts** (how much memory, which
persistence, which eviction policy, which ACL users), and the service **translates**
them into a detailed `redis_config` (full `redis.conf`). The translation happens via
CEL `merge()`: author defaults + persistence preset + computed `maxmemory` +
operator passthrough directives, SHALLOW last-wins ([templating.md §2.3](../../../docs/templating.md)).

> **Status (2026-06-25).** The service deploys **two** modes - **`sentinel`**
> (master-replica + sentinel daemon) and **`cluster`** (honest hash-slot Redis Cluster,
> 16384 slots) - both in the `create` scenario, selected via `redis_type`
> ([`cluster.yml`](scenario/create/cluster.yml) and
> [`sentinel.yml`](scenario/create/sentinel.yml) are wired into the dispatcher). **The
> `standalone` and `sentinel_only` modes have been removed from the service** (user
> decision 2026-06-25): standalone deployment is covered by `sentinel` with
> `replicas_per_master: 0` (a single master + sentinel daemon), while the thin sentinel
> layer over an external master (`sentinel_only`) remains a **capability of the
> [`redis`](../../destiny/redis/) destiny brick** (`deploy_redis` / `sentinel_enabled`
> flags) for reuse by other services - e.g. DragonFly - but not as a mode of this
> service. Narrowing the `redis_type` enum to `[sentinel, cluster]` - bump
> `state_schema_version` 3→4 + migration [`003_to_004`](migrations/003_to_004.yml)
> (forward-only remap of the old modes, see [state_schema](#state_schema)).
>
> Day-2 cluster operations are implemented - node join/eviction/resharding via
> separate scenarios [`add_node`](scenario/add_node/main.yml) /
> [`remove_node`](scenario/remove_node/main.yml) /
> [`reshard`](scenario/reshard/main.yml) and rolling-restart
> [`restart`](scenario/restart/main.yml), as well as day-2 hot-reload
> [`update_config`](scenario/update_config/main.yml) /
> [`add_user`](scenario/add_user/main.yml) /
> [`update_users`](scenario/update_users/main.yml) /
> [`rotate_tls`](scenario/rotate_tls/main.yml). Optionally, `create` can, in the
> **same run**, provision VMs for the topology via cloud-provision (`input.provision`,
> state_schema bumped 4→7, shared body [`redis-provision.yml`](scenario/redis-provision.yml))
> - see [Cloud-provision](#cloud-provision-create-provisions-vms-live-awaits-c1).
> **★ Cloud-provision - keeper-side implemented, live provisioning awaits C1 + live-e2e.**
> Bootstrap-token delivery is implemented keeper-side (module `core.bootstrap.delivered`,
> [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md), SSH delivery). The
> flow is valid at render time (L0 Trial) and passes unit tests, but live "VM→redis in
> one run" provisioning still **will not pass live-e2e** without slice **C1** (cloud-init
> generates a CA-signed host key so Keeper can verify the fresh VM's host cert without
> TOFU) - details in the section. An unknown `redis_type` (outside the enum) is
> rejected by **Keeper input validation BEFORE rendering** (a clear failure, the run
> never starts) - the former shell-mode-guard has been removed. The rest of the
> backlog (sentinel failover/day-2, plugin `failover`, sentinel daemon TLS) - see
> [In progress](#in-progress). The README describes only what exists in the files.

Division of responsibilities (architect B-hybrid, ADR-009):

- **destiny [`redis`](../../destiny/redis/)** - a mode-agnostic per-host brick:
  installs `redis-server` (dispatched by `install.method`: distro package **or**
  standalone binaries from Nexus), renders `redis.conf` from an **already merged**
  config, renders `users.acl`, TLS PEM (`core.file.present` + `${ vault(ref) }` in the
  cell), systemd-hardening drop-in, host-tuning extras (THP/logrotate/sysctl), starts
  the service. destiny is "dumb" - it doesn't merge or orchestrate anything itself;
  the merge is done by the service scenario.
- **service scenario** - translation of simple input into `redis_config` (via
  `merge()`) + orchestration (step order, targeting, health-gate, unrolling the
  cluster topology into a `nodes` MAP; in future batches - rolling-restart, day-2).
- **plugin [`community.redis`](../../../docs/module/community/redis/README.md)**
  (binary `soul-mod-community-redis`) - the **primary interface** to a **live** Redis
  (`CONFIG SET`, ACL, cluster, sentinel, failover, raw commands). Wired in via
  [`service.yml → modules[]`](service.yml).

## state_schema

[`service.yml → state_schema`](service.yml), `state_schema_version: 11`. Forward-only
migration chain (ADR-019):

- [`001_to_002.yml`](migrations/001_to_002.yml) - `redis_users` from a list of names into
  a map `name → {perms, state}`;
- [`002_to_003.yml`](migrations/002_to_003.yml) - "richer state": the opaque
  `redis_config` is supplemented with named intent fields (day-2 read-model:
  `tls`/`install`/`persistence`/`memory_mb`/`maxmemory_policy`/`modules` + topology
  `shards`/`replicas`/`sentinel_quorum`);
- [`003_to_004.yml`](migrations/003_to_004.yml) - **narrowing the `redis_type` enum to
  `[sentinel, cluster]`**: forward-only remap of the old modes `standalone → sentinel`
  and `sentinel_only → sentinel`, so state stays schema-valid after these modes were
  removed from the service. The migration file itself carries the `needs_architect`
  flag (★★): the remap changes the live meaning of the incarnation (standalone had no
  sentinel daemon; `sentinel_only` had no data plane) - correctness of the target mode
  for live standalone/`sentinel_only` incarnations must be confirmed before production;
- [`004_to_005.yml`](migrations/004_to_005.yml) - sentinel failover timings:
  `redis_sentinel` is supplemented with `down_after_ms`/`failover_timeout_ms` (day-2
  read-model, default `5000`/`60000`); only for sentinel incarnations, no-op for cluster
  (`redis_sentinel {}`);
- [`005_to_006.yml`](migrations/005_to_006.yml) - `redis_users` from a map
  `name→{perms,state}` into a **typed array `[{name, perms, state}]`** (ADR-062 `AclUser`):
  the map key (name) moves into the `name` field;
- [`006_to_007.yml`](migrations/006_to_007.yml) - **cloud-provision read-model**
  ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md), Option A):
  state gets two independent fields, `provisioned_vm_ids` (array string) and
  `provisioned_provider` (string). Existing incarnations (deployed **without** provision)
  get a conservative default `[]` / `''` (hosts are a declared roster, not provisioned by
  the service); two flat `set`s with a `has()` guard (back-compat + idempotency on rerun);
- [`007_to_008.yml`](migrations/007_to_008.yml) - **cascade-destroy read-model**
  (addition to v7): state gets `provisioned_sids` (array string) - Keeper-side SID/FQDN
  of VMs raised by our run. Day-2 teardown reads them to cascade-clean the
  `souls`/`soul_seeds`/`bootstrap_tokens` registries (the Reaper does not remove
  pending-VM-souls itself). Pairs with `provisioned_vm_ids`: `vm_ids` - provider id (for
  `core.cloud.destroyed`), `sids` - Keeper Soul id (for cascade) - different subsystems.
  One flat `set`, default `[]` for existing incarnations (`has()` guard);
- [`008_to_009.yml`](migrations/008_to_009.yml) - **seed-source read-model** (pilot
  `migrate_cluster`): state gets a nested object `seeded_from` - **where** the data was
  seeded from during migration from an external cluster: `source_endpoints` (array
  string) and `detached` (bool - `false` after migrate, `true` after day-2
  `detach_source`). Time is deliberately not in state (L0 determinism; `now()` is
  forbidden in migration-CEL) - it's taken from `state_history`. For a plain `create`
  (not a migration), a conservative default `{source_endpoints: [], detached: false}`;
  a single `set` with a `has()` guard on the **whole object**;
- [`009_to_010.yml`](migrations/009_to_010.yml) - `seeded_from` gets **references** to
  the external source's credentials: `source_password_ref` (Vault **path** to the
  password) + `source_tls_ca_ref` (Vault path to the CA), both strings. Production
  blocker fix: `detach_source` performs a final offset-gate against the **external**
  source, but previously the source credentials weren't stored in state (only in
  `input.source.*` on `migrate_cluster`) → connecting to the AUTH/TLS source failed →
  detach never succeeded (`error_locked`). Now `migrate` **persists the references**
  (not secrets - Vault paths), `detach` reads them via `vault()` keeper-side. For
  existing v9 incarnations (and plain `create`) - empty refs (source without AUTH/TLS).
  A **leaf guard** (`has()` on each new field inside the already-existing
  `seeded_from`), not an object guard like `008_to_009` - otherwise it would have
  overwritten `source_endpoints`/`detached`;
- [`010_to_011.yml`](migrations/010_to_011.yml) - **redesign of the `create` input
  contract** (`connection_mode` + `io_threads` + extended `persistence` + restructured
  sentinel timings). Four independent state transformations (best-effort - a migration
  is a pure function of the old state, ADR-019: `essence`/`input`/`vault` are forbidden
  in migration-CEL):
  (1) **persistence REMAP** of the old enum `[off, aof, rdb, rdb_aof]` into the new one -
  `aof → aof_1sec`, `rdb_aof → rdb_aof_1sec` (the old `aof`/`rdb_aof` were fsync
  `everysec` bit-for-bit), `off`/`rdb` - identity;
  (2) **`connection_mode`** (new, enum `tls`/`tls_plain`/`plain`) - a reverse fold from
  the existing namedfield `tls.{enable,only}`: `!enable → plain`, `enable && only → tls`,
  `enable && !only → tls_plain`;
  (3) **`io_threads`** (new, int) - best-effort default `0` (the original I/O thread
  count wasn't held in namedfields - unrecoverable from `redis_config`, like
  `memory_mb` in `002_to_003`);
  (4) **`redis_sentinel` RESTRUCT** (sentinel incarnations only): the named
  `down_after_ms`/`failover_timeout_ms` are folded into `master_settings`
  (`map<string,string>` of effective per-master directives - mirroring
  `input.sentinel_master_settings`) + `settings: {}` (global sentinel settings -
  mirroring `input.sentinel_settings`); for cluster (`redis_sentinel == {}`) → no-op.
  The exact field value is restored on the next `create` override.

`incarnation.state` records what has been deployed so the operator can see the
installation, and so a repeated apply stays idempotent:

| field | type | meaning |
|---|---|---|
| `redis_type` | enum `sentinel`/`cluster` | deployment mode (both implemented) |
| `redis_version` | string | effective Redis version from input `version` (enum of builds published to Nexus - `8.6.1`/`8.4.0`/`8.2.2`/`8.0.3`/`7.4.1`/`6.2.21`). `install_method=binary` (default) downloads binaries of this version from Nexus; `install_method=package` pins `=version`; see input `version` / `install_method` |
| `connection_mode` | enum `tls`/`tls_plain`/`plain` | **(v11)** network channel mode: `plain` - plain port only (TLS off); `tls_plain` - TLS and plain simultaneously; `tls` - TLS only (plain closed, `port 0`). Day-2 read-model (replaced the boolean `tls_enabled`/`tls_keep_plain`) |
| `io_threads` | integer | **(v11)** number of Redis I/O threads (`io-threads` directive). `0` - single-threaded I/O (directive not written) |
| `redis_config` | object | **the translation result** - merged `redis.conf` config (default → preset → computed → passthrough; for `cluster` - plus `cluster-*` directives) |
| `redis_users` | array `AclUser` (`[{name, perms, state}]`) | **operator-extra** Redis ACL users (operator-created only). Element is a typed `AclUser` (`name` + `perms` required, `state` defaults to `on`) from [`types.yml`](types.yml), reusable via `$type: AclUser` in scenario `input:` (ADR-062). Prior to state_schema v6 this was a map `username → {perms, state}` - migration [`005_to_006.yml`](migrations/005_to_006.yml) folded map→array (name key → `name` field). `perms` is the full ACL string (passwords are NOT in state - keeper-side Vault). **System** service users (`replica`/`monitoring`/`sentinel`/`haproxy`, etc.) are **NOT** written here - they're merged into `users.acl` from `essence.system_acl_users` on every render (see [System ACL users](#system-acl-users)) |
| `redis_hosts` | array `{sid, role}` | topology hosts (written as `[]`; the exact `primary`/`replica`/`sentinel` roles for cluster/sentinel are laid out on the apply side - not recorded in state) |
| `redis_sentinel` | object `{master_name, quorum, master_settings, settings}` | sentinel-mode facts: monitored master name (from `essence.sentinel_master_name`, default `master`) + quorum + `master_settings` (effective per-master sentinel directive dict = `essence.sentinel_master_defaults` overridden by `input.sentinel_master_settings`, `map<string,string>`; defaults `down-after-milliseconds`=`5000`/`failover-timeout`=`60000`) + `settings` (global passthrough `sentinel.conf` directives from `input.sentinel_settings`). `quorum` is always `0` (the auto `size/2+1` is computed in apply, not materialized in state). **(v11 restruct)**: the former named `down_after_ms`/`failover_timeout_ms` (v5) are folded into `master_settings`. Outside `sentinel` mode - an empty object |
| `provisioned_vm_ids` | array string | **(v7, cloud-provision read-model, [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md))** provider vm-ids of VMs raised **by this** create run via `core.cloud.created` (from `register.provision.vm_ids`). Day-2 teardown reads them for `core.cloud.destroyed`. Without provision (`input.provision` omitted/`enabled:false`) - `[]` (hosts are a declared roster, not provisioned by the service). See [Cloud-provision](#cloud-provision-create-provisions-vms-live-awaits-c1) |
| `provisioned_provider` | string | **(v7)** cloud Provider name in the registry (used by the same `destroy` call; from `input.provision.provider`). Without provision - `''` |
| `provisioned_sids` | array string | **(v8)** Keeper-side SID/FQDN of VMs raised **by this** run. `destroy` reads them to cascade-clean the `souls`/`soul_seeds`/`bootstrap_tokens` registries (read **paired** with `provisioned_vm_ids` - lengths must match, otherwise orphan; `destroy` carries an `assert` guard on the pairing). Without provision - `[]` |
| `seeded_from` | object `{source_endpoints, detached, source_password_ref, source_tls_ca_ref}` | **(v8/v10)** **where** the data was seeded from during `migrate_cluster`: `source_endpoints` (array string - external source), `detached` (bool - `true` after `detach_source`), `source_password_ref`/`source_tls_ca_ref` (Vault **paths** to the source's credentials, **not** secrets - v10). For plain `create` (not a migration) - `{source_endpoints: [], detached: false}` + empty refs |

Besides the fields listed, `state_schema` carries **named intent fields** (read-model,
v3): `tls` / `install` / `persistence` / `memory_mb` / `maxmemory_policy` / `modules`
(read-model of deployed modules) / `modules_base_url` / `conf_dir` / `data_dir` /
`sysctl_settings` + counts `shards` / `replicas` / `sentinel_quorum`. They're kept
consistent with `redis_config` by construction (a single `compute` pass - see
[Translation](#translating-simple-input-into-redis_config)), and day-2 scenarios read
them **by name**, without parsing `redis_config`.

`required: [redis_type, redis_config]`.

## Operator input contract

[`scenario/create/main.yml → input:`](scenario/create/main.yml) - strictly typed
structural input (Named Dict, each parameter has a definite type), **not** free-form
text:

| field | type | meaning |
|---|---|---|
| `redis_type` | enum `sentinel`/`cluster`, default `sentinel` | mode; selects the dispatcher branch. `sentinel` - master-replica + sentinel daemon (with `replicas_per_master: 0` - the standalone equivalent); `cluster` - honest hash-slot Redis Cluster. A value outside the enum is rejected by Keeper input validation BEFORE rendering |
| `version` | string, **required**, enum `8.6.1`/`8.4.0`/`8.2.2`/`8.0.3`/`7.4.1`/`6.2.21` | Redis version from a fixed set of builds published to Nexus (the free-form distro pin was replaced with a closed enum - redesign). `install_method=binary` (default) downloads binaries of this version from Nexus; `install_method=package` pins `=version` with the same value. An arbitrary version is not accepted |
| `install_method` | enum `package`/`binary`, default `binary` | redis install method. `binary` (DEFAULT) - standalone `redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` binaries from Nexus (`essence.binary_base_url` + per-host `arch`/`Debian`/`distro_ver`/`version` from `soulprint.self.os`; downloaded with SHA-256 content-idempotency, without integrity verification). `package` - distro package `redis-server` (`=version` pin). The binary URL/version is NOT operator input: it lives in `essence` (`binary_base_url`/`binary_version`, author context), the operator overrides it in `spec.essence`. The former object-input `install` was collapsed into a single enum field |
| `memory_mb` | integer, optional, min `64` | memory budget for Redis on the host, MB; `maxmemory` is a fraction of it |
| `io_threads` | integer, optional, default `0`, min `0` | number of Redis I/O threads (`io-threads` directive). `0` (default) - single-threaded I/O (directive not written); `>0` - Redis parallelizes socket handling (typically = core count − 1). Written to `redis.conf` only when `>0` |
| `persistence` | enum `off`/`aof_1sec`/`aof_always`/`rdb`/`rdb_aof_1sec`/`rdb_aof_always`, default `rdb` | durability mode; translated into `save`/`appendonly`/`appendfsync`. `off` - no persistence; `aof_1sec`/`aof_always` - AOF only (fsync `everysec` / `always`); `rdb` - snapshots only; `rdb_aof_1sec`/`rdb_aof_always` - snapshots + AOF (dual durability). Extended from `[off, aof, rdb, rdb_aof]` (redesign: explicit choice of fsync frequency `1sec`/`always`) |
| `maxmemory_policy` | enum of eviction policies | eviction policy when `maxmemory` is reached |
| `replicas_per_master` | integer, optional, default `2`, min `0`, max `8` | **(both modes)** replicas per master. `cluster` - replicas per shard (roster = `shards * (1 + replicas_per_master)`); `sentinel` - replicas of the master (roster = `1 + replicas_per_master`). default `2` (HA set: master + 2 replicas); `0` - no replicas (`sentinel` with `0` = the standalone equivalent); ceiling `8`. Unified 2026-06-25 (replaced the former `replicas` + `replicas_per_shard`) |
| `shards` | integer, required for `cluster` (`required_when`), min `3` | **(cluster)** number of master shards; the 16384 hash slots are split evenly across masters. `min 3` is the lower bound for an honest Redis Cluster (it won't form from `<3` masters). Required when `redis_type=cluster` via `required_when: "input.redis_type == 'cluster'"` - omitting it fails input validation BEFORE rendering |
| `cluster_node_timeout` | integer, optional, default `5000`, min `1` | **(cluster)** gossip timeout between nodes, ms (`cluster-node-timeout` directive) |
| `cluster_topology` | array of array of string, optional | **(cluster)** explicit shard layout: a list of shards, each shard a list of SIDs `[master, replica, …]` (the first is the master). The operator distributes VMs across zones / encodes anti-affinity themselves. If not set → the plugin lays out roles automatically by sorting SIDs (behavior identical to before the feature). Each SID must appear exactly once among the targeted souls; all roster hosts must be covered |
| `users` | array `AclUser` (`[{name, perms, state}]`) | **operator-extra** ACL users; element is a typed `AclUser` ([`types.yml`](types.yml), ADR-062 `$type`): `name` + `perms` required, `state` ∈ `on`/`off` (default `on`). `perms` is the full Redis ACL string, validated by the re2 `AclUser` pattern (token-shape filter). The name **cannot** collide with a system service user (`default`/`replica`/`monitoring`/`sentinel`/`haproxy`) - the collision is rejected by input validation (422, see [System ACL users](#system-acl-users)). Merged **on top of** the system users (last-wins) into `users.acl` |
| `redis_settings` | object (passthrough, key→value strings) | arbitrary `redis.conf` key→value directives; **override everything** in the final merge. Directive **names** are version-aware validated with an `assert` against the valid-directive catalog for the chosen Redis version (see [redis_settings directive-name validator](#redis_settings-directive-name-validator)) - a typo / a directive from a different version fails the run at render time (422) BEFORE applying |
| `connection_mode` | enum `tls`/`tls_plain`/`plain`, default `plain` | **(network channel)** Redis channel mode (replaced the boolean `tls_enabled`/`tls_keep_plain` - redesign). `plain` (default) - plain port 6379 only (TLS off); `tls_plain` - TLS and plain simultaneously; `tls` - TLS only (plain port closed, `port 0`). The enum **structurally** excludes "no listener at all". Technical TLS parameters (port, Vault paths `cert`/`key`/`ca`) are NOT operator input: they live in `essence` (`tls_port`/`tls_cert_ref`/`tls_key_ref`/`tls_ca_ref`, author context). destiny reads the PEM via `vault(ref)` in the `content` cell (seal masking) |
| `sentinel_settings` | object (passthrough, key→value strings), optional | **(sentinel)** global (not per-master) passthrough `sentinel.conf` directives (`resolve-hostnames`, `announce-port`, …), not covered by `sentinel_master_settings`. Merged into `sentinel.conf` alongside the per-master directives. No version validation (there is no sentinel catalog). New field (redesign) |
| `sentinel_master_settings` | object (passthrough, key→value strings), optional | **(sentinel)** per-master Sentinel directives: key is the `SENTINEL SET` option name without the prefix and master name (`down-after-milliseconds`, `failover-timeout`, `parallel-syncs`, …); value is a string. Overrides the `essence.sentinel_master_defaults` defaults (`down-after-milliseconds`=`5000`, `failover-timeout`=`60000`). **Absorbed** the former named fields `sentinel_down_after_ms`/`sentinel_failover_timeout_ms` (redesign) |
| `provision` | object `{enabled, provider, profile, ssh_provider, await_timeout}`, optional | **★ keeper-side implemented, live awaits C1** ([Cloud-provision](#cloud-provision-create-provisions-vms-live-awaits-c1)): provision VMs for the topology in the **same** create run ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md), Option A). `enabled: true` → cloud-create + token delivery + onboarding barrier, then the redis role on the newly created hosts; omitted / `enabled: false` → run against the existing roster (behavior bit-for-bit identical to without the feature). `provider`/`profile` - names in the `providers`/`profiles` registries; `ssh_provider` - the SshProvider plugin name for token delivery (`core.bootstrap.delivered`, [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)); `await_timeout` (`<N>{s\|m\|h}`) - ceiling for onboarding wait (unset → `essence.provision_await_timeout`, default `10m`). **There is no VM count field in `provision`** - it's derived from the topology (`shards`/`replicas_per_master`). **Live provisioning awaits slice C1** (cloud-init CA-signed host key) + live-e2e - see the section |
**What's missing from the input contract (essence parameters or auto-computed):**

- `sentinel_quorum` - **auto** `size(hosts)/2+1` (majority), computed in apply.
  There is no longer an operator field for it (removed 2026-06-25).
- `sentinel_master_name` - moved to `essence.sentinel_master_name` (author context,
  default `master`; the operator overrides it in `spec.essence`, not in the Run form).
- `sentinel_down_after_ms` / `sentinel_failover_timeout_ms` - **absorbed** into the
  `sentinel_master_settings` dict (keys `down-after-milliseconds` / `failover-timeout`);
  their defaults `5000`/`60000` moved to `essence.sentinel_master_defaults` (the base
  layer under that dict). Redesign: named fields replaced by a passthrough dict.
- `conf_dir` / `data_dir` - moved to `essence` (`conf_dir`=`/etc/redis`,
  `data_dir`=`/var/lib/redis`, author context; the operator overrides the layout in
  `spec.essence` for their own storage layout, not in the Run form). They remain in
  `state` (day-2 `add_node` reads the layout from `incarnation.state`) - the source of
  the edit shifted input→essence, the state shape is unchanged.
- technical TLS parameters (`tls_port`, Vault paths `tls_cert_ref`/`tls_key_ref`/`tls_ca_ref`)
  - in `essence` (author context), not operator input: the operator only chooses the
  channel mode via `connection_mode`, while port `7379` and the PEM paths are
  overridden in `spec.essence`.
- `tls_enabled` / `tls_keep_plain` - the boolean flags were **removed**, replaced by the
  enum `connection_mode` (`tls`/`tls_plain`/`plain`); `compute.tls_on`/`tls_only_on` are
  derived from the enum.
- `modules` (set of Redis modules) - no longer an **operator choice**: the directive is
  "modules are **always all**" - destiny deploys the full set from its own
  `vars.redis_modules` on Redis < 8; on Redis 8+ the modules are built in (`.so` files
  are not downloaded). A subset cannot be selected.
- `modules_base_url` (source of `.so` files) - moved to `essence.modules_base_url`
  (author context, default `""`; the operator overrides it in `spec.essence` for their
  own mirror). On Redis < 8 without a configured source, destiny won't be able to build
  the `.so` URL (surfaces softly at destiny render time, not at input validation -
  the `validate` context is input-only, essence is unavailable there).
- `master_ip` / `master_port` - belonged to the removed `sentinel_only` mode (an
  external master). In `sentinel`, the master is chosen from the roster.

**Cross-field input invariants** ([`create/main.yml → validate:`](scenario/create/main.yml),
input-only, first failure → 422 `validation_failed` BEFORE committing the incarnation
and BEFORE applying):

- the name in `input.users` **is not** among `[default, replica, monitoring, sentinel, haproxy]`
  - operator-extra cannot take the name of a system service user (it would silently be
  overridden by the merged-in system perms; see [System ACL users](#system-acl-users)).
  The same guard exists in `add_user` ([`validate:`](scenario/add_user/main.yml)).

> **Two former `validate` rules were removed by the redesign.** (1) "at least one
> listener" (`tls.only` required `tls.enable`) - the `connection_mode` enum
> `[tls, tls_plain, plain]` **structurally** excludes "no listener at all" (you can't
> pick "TLS off + plain closed"). (2) "`install.method=binary` requires `base_url`/
> `version`" - the binary URL and version moved from operator input into `essence`
> (`binary_base_url`/`binary_version`, author context), and the `validate` context is
> input-only (essence is unavailable); an empty `binary_base_url` with
> `install_method=binary` surfaces at destiny render time, not at input validation.

There are **no** `logrotate_enable` / `sysctl_enable` / `thp_disable` parameters in the
input contract: disabling Transparent Huge Pages, the logrotate config, and sysctl
tuning are an **unconditional baseline** (Redis recommendation / hardening, aligned with
the Ansible role's sysctl block), not an operator choice. These tasks are always active
and reach destiny via `apply.input`. Details - [Host-tuning extras](#host-tuning-extras).

The topology of both modes is specified with **counters**, not a host list. cluster -
`shards` + `replicas_per_master`: the expected roster is
`shards * (1 + replicas_per_master)` souls, and the plugin lays out roles against the
actual roster itself (see [`create` (cluster mode)](#create-cluster-mode)). sentinel -
via the `replicas_per_master` counter (roster `1 + replicas_per_master`, see
[`create` (sentinel mode)](#create-sentinel-mode)); with `replicas_per_master: 0` that's
one master + sentinel daemon (the standalone equivalent). There is **no** render-time
guard checking that the number of targeted souls matches these counters - a mismatch
surfaces when the plugin assembles the topology, not fail-fast at render time.
Cloud-creation of machines for the topology is post-beta.

Passwords are **not part of the input contract** - they live in Vault, resolved
keeper-side (see [Security](#security)).

### Example of simple input - sentinel single-host / standalone equivalent (as in the L0 case)

From [`create/tests/full-stack/case.yml`](scenario/create/tests/full-stack/case.yml)
(`replicas_per_master: 0` → a single host = master + sentinel daemon):

```yaml
input:
  redis_type: sentinel
  version: "7.4.1"
  install_method: package
  replicas_per_master: 0
  memory_mb: 1024
  persistence: rdb
  maxmemory_policy: volatile-lru
  users:
    - { name: app, perms: "~app:* +@read +@write -@dangerous", state: "on" }
  redis_settings:
    timeout: 60
    tcp-backlog: 511
```

### Example of simple input - cluster (as in the L0 case)

From [`create/tests/cluster-create-2shards-1replica/case.yml`](scenario/create/tests/cluster-create-2shards-1replica/case.yml)
- `2 * (1 + 1) = 4` hosts in the run's roster:

```yaml
input:
  redis_type: cluster
  version: "7.4.1"
  shards: 2
  replicas_per_master: 1
  cluster_node_timeout: 8000
```

The operator declares **only counters**: 2 shards of 1 replica each. The layout of
master/replica roles and hash slots is done by the plugin; the operator doesn't list
hosts by name - they're taken from the run's targeted roster.

### Example of simple input - sentinel (as in the L0 case)

From [`create/tests/sentinel-create-1master-2replica/case.yml`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
- `1 + replicas_per_master = 1 + 2 = 3` hosts in the run's roster:

```yaml
input:
  redis_type: sentinel
  version: "7.4.1"
  replicas_per_master: 2
```

The operator declares **only the counter** `replicas_per_master` (1 master + 2
replicas). Master election (who is master) - the first host by SID; the apply side
handles role layout and replica/sentinel binding. `sentinel_quorum` auto = the
majority `size(hosts)/2+1`, there is no operator field for it; `sentinel_master_name`
comes from `essence.sentinel_master_name` (default `master`).

## Translating simple input into redis_config

The service merges four layers via `merge()` (SHALLOW last-wins, left to right - the
right side overrides the left by top-level key):

1. `essence.redis_config` - author defaults (role defaults);
2. `essence.persistence_presets[persistence]` - `save`/`appendonly` for the chosen mode;
3. computed `maxmemory` + `maxmemory-policy` - derived from `memory_mb` / input;
4. `input.redis_settings` - operator passthrough (overrides everything).

The translation data tables live in [`essence/_default.yaml`](essence/_default.yaml):
`persistence_presets`, `memory_reserve_percent`, the `redis_config` merge base layer.

Translation walked through one field at a time:

- **`persistence: rdb`** → `essence.persistence_presets["rdb"]` =
  `{save: "900 1 300 10 60 10000", appendonly: "no"}` - RDB snapshots enabled, AOF disabled.
- **`memory_mb: 1024`** → `maxmemory = 1024 * memory_reserve_percent(75) / 100 = 768`
  → directive `maxmemory: "768mb"` (25% reserve - for the OS/overhead). If unset,
  `maxmemory` is taken from the `essence.redis_config` merge base layer.

Layer 3 only materializes the keys that were actually set (a has-guard): an empty
`memory_mb` won't produce the string `"0mb"`, a missing `maxmemory_policy` won't
overwrite the default/preset.

## Host-tuning extras

Mode-agnostic per-host additions, shared across all modes - implemented in
[`destiny/redis/tasks/extras.yml`](../../destiny/redis/tasks/extras.yml). All three
are an **unconditional baseline** (Redis recommendation / hardening, not an operator
choice): there are no operator flags, the tasks are **always** rendered.

- **THP - unconditional.** Disabling Transparent Huge Pages - drop-in hardening
  (a oneshot systemd unit `disable-thp.service` + `core.service.running enabled`),
  always rendered. This is a Redis recommendation (THP causes latency spikes on
  RDB/AOF fork), not an operator choice - hence there is no `thp_disable` parameter
  in the input contract.
- **logrotate - unconditional.** The `/etc/logrotate.d/redis` config (rotation of
  `/var/log/redis/*.log`, `copytruncate`), always rendered. The former
  `logrotate_enable` opt-out flag no longer **exists** - setting `logrotate_enable: false`
  is rejected by Keeper input validation as `unknown_key`.
- **sysctl - unconditional.** A single `core.sysctl.applied` step (state `applied`):
  the module itself builds a deterministic drop-in `/etc/sysctl.d/30-redis.conf` from
  a map (sorted keys) and reactively reloads it with `sysctl -e -p <file>` (targeted at
  the drop-in, NOT the entire `--system`; `-e` suppresses read-only/nonexistent keys in
  containers without failing the run). Always applied. The set and values of kernel
  parameters are **aligned 1:1 with the sysctl block of the Ansible redis role**
  (memory/fork overcommit, swappiness, network buffers, backlogs, the TCP stack); the
  source of the values is the data table
  [`essence/_default.yaml → sysctl_settings`](essence/_default.yaml) (not in operator
  input - this is Redis-specific tuning, not an operational choice). The former
  `sysctl_enable` opt-out flag no longer **exists**. The role's `tcp_bbr` block was
  **not** ported (depends on the `tcp_bbr` kernel module, not loaded by default on
  Debian) - deferred.

All three sets of values are host-invariant → they reach destiny via `apply.input`.

## System ACL users

Besides operator-extra (`input.users`), the service **always merges in** a set of
**system** ACL users into `users.acl`, without which the cluster won't work: `replica`
(`PSYNC` replication), `monitoring` (metrics exporter), `sentinel` (AUTH
sentinel↔redis), `haproxy` (load balancer health-check). Each one's perms were copied
1:1 from the Ansible redis role (`redis_system_users` / `redis_sentinel_system_users`)
and live in [`essence/_default.yaml`](essence/_default.yaml) (`system_acl_users` - for
`redis-server`; `system_acl_users_sentinel` - for the sentinel daemon). The operator
does **not set** and does **not see** them in the input form - this is author-context
essence.

### Merge: system users at the bottom, operator-extra on top

In **all** tasks that render `users.acl` (create cluster/sentinel + day-2
`add_user`/`update_config`/`add_node`), `apply.input.users` is assembled with a double
`merge()`: **first** the system users from `essence.system_acl_users` (bottom layer,
`state: on`, password from Vault), **on top** - operator-extra (`input.users` on
create / `state.redis_users` on day-2 / `compute.users_new` in `add_user`). The order
of `merge()` arguments sets priority: last-wins, so operator-extra would override a
system user of the same name - but the name collision is closed off by the
validate-guard (below), so the layers never actually overlap.

The set is accessed via `default(essence.system_acl_users, {})`: **back-compat** -
incarnations with old essence that lacks this field do NOT get the system users (the
feature is enabled by **updating essence**, not by scenario code).

### ★ Re-merging on day-2 (invariant)

`add_user` / `update_config` / `add_node` re-render the **entire** `users.acl` (not an
append) with the same destiny brick. So each of these tasks **must** re-merge the
system users - otherwise the re-render would **wipe out**
`replica`/`monitoring`/`sentinel`/`haproxy` and break replication / sentinel↔redis /
health-check on day-2. The system users are **not stored in state** (state only holds
operator-extra), so they're merged in from `essence` on **every** render. `add_node` is
especially critical: without the system `replica`, a new node won't be able to
`PSYNC` - the cluster won't accept it as a replica.

### The default user: split across two aclfiles

- **redis-server** - `default` is **absent** from `system_acl_users`: it's set via
  `requirepass` in `redis.conf` (`users.acl` doesn't duplicate it, same as in the
  Ansible role).
- **sentinel daemon** - `default` **is present** in `system_acl_users_sentinel`: the
  sentinel daemon has no `requirepass` equivalent for aclfile access, so `default` is
  declared in `sentinel-users.acl` with the same primary secret
  `secret/redis/<incarnation>#password` (the same as redis-server's requirepass -
  otherwise the sentinel daemon wouldn't let the default client in with the shared
  password).

### The sentinel daemon's second aclfile

The sentinel daemon requires a **separate** perms set (`sentinel|*` commands, not
regular redis commands), so it's written into a **second** aclfile -
[`sentinel-users.acl`](../../destiny/redis/templates/sentinel-users.acl.tmpl) (pointed
to by `aclfile` in `sentinel.conf`). Only in `sentinel` mode does the scenario pass
destiny a new additive field `input.sentinel_users` (from
`essence.system_acl_users_sentinel`); operator-extra is **not** merged in here
(sentinel access is service-internal). Passwords: `default` → the primary
`secret/redis/<incarnation>#password`; the rest (`monitoring`/`sentinel`/`haproxy`) →
the branch `secret/redis/<incarnation>/users/<name>#password`. For the full render
mechanics see
[destiny README → System ACL users and the second aclfile](../../destiny/redis/README.md#system-acl-users-and-the-second-aclfile).

### validate-guard on reserved names

operator-extra (`input.users` on create, `input.user.name` in `add_user`,
`input.users[].name` in `update_users`) **cannot** take a system name:
`[default, replica, monitoring, sentinel, haproxy]` are reserved. The collision is
caught by input validation (`validate:`, [create](scenario/create/main.yml) +
[add_user](scenario/add_user/main.yml) + [update_users](scenario/update_users/main.yml))
- **422 `validation_failed` BEFORE applying**, rather than a silent override (an
operator `replica` with `~* +@all` would silently grant extra privileges and break
replication's replica perms). The guard is **input-only**: the name list is hardcoded
in each scenario (essence is unavailable in the `validate` context - a structural CEL
barrier).

## Scenarios

### `create` (single entry point, dispatched by `redis_type`)

The operator always calls **`create`**; the `redis_type` field selects the mode.
[`scenario/create/main.yml`](scenario/create/main.yml) is the **dispatcher**: the run
body is split into two branches, wired in via `include:` -
[`cluster.yml`](scenario/create/cluster.yml) and
[`sentinel.yml`](scenario/create/sentinel.yml). `include:` is expanded
**unconditionally** into a flat list BEFORE render (`when:` on the include task itself
is forbidden), so branching isn't done by include but by a `when:` on
`input.redis_type` on each task in the branch: the predicate is static (only
`input.*`), Keeper evaluates it at render time and suppresses the inactive branch with
a placeholder-skip (ADR-012(d) Option b). Include order - cluster, sentinel: branches
are appended at the tail, previous branches' indices don't shift.

**An unknown mode - input validation, not a shell-guard.** The former `core.cmd.shell`
"mode-guard" first task has been **removed**: an unknown `redis_type` is rejected by
**Keeper's enum input validation** for `input.redis_type` (`[sentinel, cluster]`)
**BEFORE rendering** - earlier and clearer than a shell test on Soul. Both branches are
gated on their own `redis_type`, and the enum guarantees exactly one branch matches, so
the footgun "unimplemented mode = a silent green no-op" is structurally impossible (a
value outside the enum never reaches render).

After a successful apply, Keeper records `state_changes` (ADR-009 §7.1, ADR-057):
`redis_type`, `redis_version`, `redis_config` (the same `compute.redis_config` that
went to render - a single source of truth; for cluster, plus `cluster-*` directives,
except the host-variant `cluster-announce-ip`, which is not written to state),
`redis_users` (from `input.users`), `redis_hosts = []`, `redis_sentinel`
(`{master_name, quorum}` in sentinel mode; otherwise an empty object) + the named
read-model fields
(`tls`/`install`/`persistence`/`memory_mb`/`maxmemory_policy`/`modules`/`modules_base_url`/`conf_dir`/`data_dir`/`sysctl_settings`
+ counts `shards`/`replicas`/`sentinel_quorum`).

#### `create` (cluster mode)

[`scenario/create/cluster.yml`](scenario/create/cluster.yml) - an honest hash-slot
Redis Cluster (16384 slots). **This is a thin dispatcher branch**: a size-guard
(below) + a single `- include: redis-deploy-cluster.yml`. The actual deploy body has
been moved to **service level**
([`scenario/redis-deploy-cluster.yml`](scenario/redis-deploy-cluster.yml)) and is
included **by filename** (scenario-include falls back `scenario/<name>/<file>` →
`scenario/<file>`, [orchestration.md §6](../../../docs/scenario/orchestration.md)) -
`migrate_cluster` (the joinable variant) shares the same body, there's no duplicate.
Deploy-body steps:

   The requirement of `shards` is **declarative**: the `input:` schema carries
   `required_when: "input.redis_type == 'cluster'"` on the `shards` field. Omitting
   `shards` in cluster mode is caught by **Keeper input validation BEFORE rendering**
   (a clear failure, the run never starts) - it's not a shell task on the host. The
   former `shards-guard` (`core.cmd.shell` with `has(input.shards)`) has been removed.

   **★ Topology size-guard - present (render-time `assert`).** The branch's first step
   ([`create/cluster.yml`](scenario/create/cluster.yml), **before** the deploy-body
   include) is an `assert:` on
   `size(soulprint.hosts) == shards * (1 + replicas_per_master)`. The predicate is
   evaluated **keeper-side at render time** (`soulprint.hosts` is available there, not
   on Soul); `false` aborts the render with a clear `message` **BEFORE any dispatch** -
   otherwise the `community.redis.cluster` plugin would have derived the shard count
   from the node count and **silently** ignored the declared `shards` (a silent
   declared↔actual desync). Case
   [`cluster-size-guard-mismatch`](scenario/create/tests/cluster-size-guard-mismatch/case.yml).
   This is **not** `required_when` (that's about a single field's presence; the
   size-guard is about reconciling with the roster). **Why `assert` and not
   `validate`:** the size-guard needs `soulprint.hosts`, and the `validate` context is
   input-only (a structural CEL barrier).

1. **`apply: destiny: redis`** - the mode-agnostic destiny, but the merged `config`
   **is supplemented with cluster directives** on top of the base merge:
   `cluster-enabled: yes`, `cluster-config-file: nodes.conf`, `cluster-node-timeout`
   (from input or default `5000`) - all three are host-invariant and correctly go
   through the config-merge (`apply.input` is resolved once on the first host by SID).
   `cluster-announce-ip` is rendered **per-host** in `redis.conf.tmpl` from
   `.self.network.primary_ip` (**this** host's IP, critical behind NAT/in the cloud),
   gated on `cluster-enabled` - NOT through the merged config: it's host-invariant
   (like `bind`), and passing it through the config map would have pinned the first
   node's IP for everyone.
2. **health-gate PING** (`community.redis.command`, `retry`) - every node must respond
   to `PING` BEFORE the cluster is assembled.
3. **cluster-build** (`community.redis.cluster`, `action: create`, `run_once` on the
   bootstrap node) - assembles the cluster. The scenario builds a deterministic
   `nodes` MAP from the run's roster (`soulprint.hosts`): key = `SID` (stable and
   sortable), value = `{addr: "<primary_ip>:6379"}`, and passes it to the plugin
   together with `replicas_per_shard` (the plugin's contract; sourced from
   `input.replicas_per_master`). The **plugin** itself performs the actual
   `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE` (via go-redis) and the split of the 16384
   slots deterministically by sorting keys - the scenario does NOT translate the
   topology, it passes a ready-made one (otherwise the two layouts would desync).
   Plugin state - in its per-module doc
   [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

#### `create` (sentinel mode)

[`scenario/create/sentinel.yml`](scenario/create/sentinel.yml) - master-replica +
sentinel daemon (with `replicas_per_master: 0` - the standalone equivalent: a single
master + sentinel daemon). **A thin dispatcher branch**: size-guard (below) + a single
`- include: redis-deploy-sentinel.yml`. The deploy body has been moved to **service
level** ([`scenario/redis-deploy-sentinel.yml`](scenario/redis-deploy-sentinel.yml))
and included **by filename** ([orchestration.md §6](../../../docs/scenario/orchestration.md))
- `migrate_cluster/sentinel.yml` shares the same body, no duplicate. Topology:
`1 master + replicas_per_master` replicas; master election is declared (a probe isn't
possible at create - redis isn't up yet), the first host by SID = master
(`soulprint.hosts[0]`, deterministically sorted). Deploy-body steps:

   **★ Topology size-guard - present (render-time `assert`).** The branch's first step
   ([`create/sentinel.yml`](scenario/create/sentinel.yml), **before** the include) is an
   `assert:` on `size(soulprint.hosts) == 1 + replicas_per_master` (the same mechanism
   as in cluster: keeper-side at render time, `false` aborts the render with a
   `message` before dispatch). Otherwise excess hosts would silently become
   undeclared replicas, and a shortfall would leave replicas missing. Case
   [`sentinel-size-guard-mismatch`](scenario/create/tests/sentinel-size-guard-mismatch/case.yml).

1. **`apply: destiny: redis`** - the same mode-agnostic destiny, but with
   `sentinel_enabled: true` → additionally renders `sentinel.conf` + the
   `redis-sentinel` unit and starts the daemon. `config` - a **base** merge (without
   cluster directives). `sentinel.master_ip` - the host-invariant master address
   (master election `soulprint.hosts[0]`), resolved `run_once` and identical for all →
   correctly passed via `apply.input`. `sentinel.master_name` - from
   `essence.sentinel_master_name` (default `master`). `announce-ip` - rendered
   per-host in `sentinel.conf.tmpl` from `.self.network.primary_ip`.
2. **health-gate PING** (`community.redis.command`, `retry`) - every node must respond
   to `PING` on `:6379` BEFORE binding replicas/configuring sentinel.
3. **REPLICAOF** (`community.redis.replica`, on the replicas - `where:` excludes the
   elected master by SID) - replicas follow the elected master. `master_addr` - the
   host-invariant address (`soulprint.hosts[0]`); `where:` guarantees the task isn't
   rendered on the master itself (a plugin guard `addr == master_addr` remains as
   defense-in-depth).
4. **SENTINEL MONITOR reconcile** (`community.redis.sentinel`, on **every** host) -
   every sentinel daemon monitors the master. `monitor.ip` - host-invariant (master
   election); `quorum` - the **auto** majority `size(hosts)/2+1` (there is no operator
   `sentinel_quorum` field). `auth_pass` - resolved keeper-side via `vault()` (masked).
   The reconcile algorithm (`MONITOR`/`REMOVE`+`MONITOR`/`SET`/`CONFIG SET`) was ported
   1:1 from Ansible. **★ Gate `when: vars.sentinel_monitor_now`** (the deploy-body's
   contract flag): on `create` the flag is **unconditionally `true`** (node-1 is the
   real master, sentinel monitors it right away). On `migrate_cluster` the same flag is
   **`false`** - the new master is still in the slave role during seeding, an early
   MONITOR would trigger a failover → split-brain; there MONITOR is **deferred** and
   executed in `detach_source` after `REPLICAOF NO ONE` (see
   [`migrate_cluster`](#migrate_cluster-day-2-migrate-from-an-external-cluster)).
5. **health-gate PONG** (`community.redis.command`, `:26379`) - every sentinel daemon
   must respond with `PONG`. **Strictly same-task** `register.self` (NOT cross-pass
   flow-control, ADR-056): "wait for N sentinels" isn't expressible - only this host's
   local sentinel is checked (`retry`/`until` + `failed_when` on
   `register.self.result == 'PONG'`).

Plugin state (states `replica` / `sentinel`, their params and idempotency) - in its
per-module doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

> **The thin sentinel layer over an external master (`sentinel_only`)** has been
> removed from the service as a distinct mode. The ability to deploy **only** a
> sentinel daemon without a local redis-server data plane remains a **capability of
> the [`redis`](../../destiny/redis/) destiny brick** (`deploy_redis: false` /
> `sentinel_enabled: true` flags) - for reuse by other services (e.g. DragonFly), but
> it is no longer invoked through the redis service.

### Cloud-provision (`create` provisions VMs, live awaits C1)

> **★★ Keeper-side implemented, live provisioning awaits C1 + live-e2e.** Per-VM
> bootstrap-token delivery is implemented keeper-side - module `core.bootstrap.delivered`
> ([ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md), SSH token delivery;
> replaced the `keeper.push.applied` stub). The flow is valid at render time (L0 Trial)
> and passes unit tests. But a live "VM→redis in one pass" run still **will not pass
> live-e2e** without slice **C1**: `push.Dial` only trusts a host-cert signed by the
> host-CA (rejecting TOFU), while a fresh VM's host key after cloud-init is still
> **bare** (not CA-signed) - the handshake during token delivery is rejected at
> connect time, the token is never delivered → the created VMs won't pass CSR
> onboarding → the `await_online` barrier (c) never reaches quorum presence →
> `error_locked`. C1 = cloud-init (B-flat userdata) generates a CA-signed host key
> with the same host-CA so Keeper can verify the host-cert. Until C1 + live
> validation, WB cloud `input.provision.enabled: true` against a live Keeper does not
> carry a run through to completion. A sample of the same flow -
> [`examples/service/example-cloud-bootstrap/`](../example-cloud-bootstrap/).

An optional `create` capability (state_schema v7, [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md),
Option A): **one** create run provisions VMs for the topology **and** deploys Redis on
them. Gated by `input.provision` ([Operator input contract](#operator-input-contract))
- set with `enabled: true` turns on the **shared, service-level provision body**
[`scenario/redis-provision.yml`](scenario/redis-provision.yml) (conditional-include
**strictly before** the cluster/sentinel branch); omitted / `enabled: false` - a run
against the existing roster, behavior bit-for-bit identical to without the feature.
The body lives **at service level** (`scenario/<file>`, not inside a single scenario)
and is included by filename by **both** consumers - `create` and `migrate_cluster`:
scenario-include resolves `scenario/<name>/<file>`, then falls back to
`scenario/<file>` ([orchestration.md §6](../../../docs/scenario/orchestration.md)), so
there is no duplicate.

**Flow** (`redis-provision.yml`, all three steps `on: keeper`):

1. **(a) cloud-create** - `module: core.cloud.created` ([keeper-side core](../../../docs/keeper/cloud.md),
   ADR-017). Creates VMs via the `soul-cloud-<provider>` CloudDriver plugin. **The VM
   count is derived from the topology** (`cluster`: `shards * (1 + replicas_per_master)`;
   `sentinel`: `1 + replicas_per_master`) - there is no separate `node_count` in
   `input.provision` (it would desync from the size formula). `generate_userdata: true`
   (ADR-017(h) B-flat): Keeper renders cloud-init from `keeper.yml::cloud_init` (CA +
   soul-binary URL); the userdata does **NOT** carry tokens. The per-VM bootstrap token
   → `register.provision.hosts[].bootstrap_token` (plain one-time, masked by
   `audit.MaskSecrets`).
2. **(b) token delivery** - `module: core.bootstrap.delivered` ([keeper-side core](../../../docs/keeper/modules.md#corebootstrapdelivered),
   [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)). Takes
   `register.provision.hosts` (sid + primary_ip + the plain bootstrap token from step
   (a)) and delivers **just the token** over SSH to each VM (the binary/CA/unit were
   already installed by cloud-init): the token goes over STDIN (not argv) →
   `/etc/soul/token` (`mode 0400`), then `systemctl start soul` (`start_soul` default
   `true`). `ssh_provider` - `input.provision.ssh_provider` (the SshProvider plugin
   name). **B1-strict**: a failure on any host → the step is `failed` → state is not
   committed → `error_locked`. **★ Implemented keeper-side, but requires C1**
   (CA-signed VM host key) - without it `push.Dial` rejects the host-cert at connect
   time (see the callout above).
3. **(c) registration + onboarding barrier** - `module: core.soul.registered` ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md)).
   `sid` - the **list** of created VMs' SIDs (`register.provision.hosts.map(h, h.sid)`,
   list-SID ADR-061); `coven` - the root `incarnation.name`. `await_online: true` blocks,
   waiting for the created Souls to go online (Redis SID-lease) within `await_timeout`;
   **B1-strict**: falling short of quorum → the step is `failed` → state is not
   committed → `error_locked`. `refresh_soulprint: true` → on success the
   scenario-runner re-resolves the roster **before** the cluster/sentinel branch
   (otherwise the branch would see an empty/stale `soulprint.hosts` - the created VMs
   wouldn't yet be in the roster).

On success, the cluster/sentinel branch rolls out the redis role onto the now-online
hosts. `state_changes` writes `provisioned_vm_ids` (from `register.provision.vm_ids`),
`provisioned_sids` (Keeper ids of the created Souls), and `provisioned_provider` (from
`input.provision.provider`) - **only** on the provision path (guarded by
`input.provision`, not `has(register.provision)`); on a non-provision run they stay
`[]` / `[]` / `''`.

**Teardown - scenario [`destroy`](#destroy-teardown-cloud-provisioned-vm).** Tearing
down provisioned VMs is implemented as a separate **lifecycle** scenario
(`scenario/destroy/`, not runnable from the Run form - a terminal flow triggered by
`DELETE /v1/incarnations/{name}`): it reads `provisioned_vm_ids` / `provisioned_sids` /
`provisioned_provider` from `incarnation.state` and calls `core.cloud.destroyed` +
cascade-cleans the registries. On a non-provisioned incarnation (deployed onto
pre-existing VMs), the `destroyed` task is group-dropped (nothing to tear down).
Details - [`destroy`](#destroy-teardown-cloud-provisioned-vm).

### `redis_settings` directive-name validator

`create` checks **every directive name** in `input.redis_settings` (passthrough into
`redis.conf`) against a catalog of valid names for the chosen Redis version - an
`assert:` on the render side ([`scenario/create/main.yml`](scenario/create/main.yml),
step "Version-aware validator"). An unknown name (a `maxmemoyr` typo, or a directive
from a different version) fails the run at render time (**422 `assert_failed`**)
**BEFORE applying**, with a clear message - rather than a late `redis-server` failure
on the host.

- **The catalog - `essence.redis_directives`** ([`essence/_default.yaml`](essence/_default.yaml)).
  ★ The name `redis_directives` is a working name (proposed in this epic). Structure:
  key = Redis `major.minor` series, value = a flat list of valid directive names for
  that series. **Six** series are covered: `6.2` / `7.0` / `7.2` / `7.4` / `8.0` / `8.2`.
- **Source - upstream Redis `src/config.c`** (the `standardConfig` table + special
  directives), **not** `redis.conf` (prose comments are indistinguishable from
  commented-out directive examples). The catalog is **committed** (there's no network
  on the render path); regeneration when adding a version -
  [`scripts/gen-redis-catalog.sh`](../../../scripts/gen-redis-catalog.sh) (from the repo
  root), the output is pasted into `essence`.
- **MVP - names only**, directive values are not validated.
- **Series-skip:** `input.version` (enum `8.6.1`/`8.4.0`/`8.2.2`/`8.0.3`/`7.4.1`/`6.2.21`)
  → the `X.Y` series is extracted with a regex; if the series **is not** in the catalog
  → the `assert` is skipped (only known series are validated, an uncataloged one isn't
  blocked). The catalog covers `6.2`/`7.0`/`7.2`/`7.4`/`8.0`/`8.2`; the `8.6`/`8.4`
  series aren't in the catalog yet → for versions `8.6.1`/`8.4.0` the validator does a
  series-skip (adding the series means regenerating the catalog). **Empty-catalog-skip:**
  no `essence.redis_directives` (old essence) → the check is skipped (back-compat).
- **Why `assert` and not `validate`:** the catalog lives in `essence`, and the
  `validate` context is **input-only** (essence is unavailable, a structural CEL
  barrier); `assert` can see `essence`+`input`+`compute`.
- **Only in `create`:** day-2 `update_config` (which also accepts `redis_settings`)
  doesn't carry this validator yet - a follow-up extension.

### `add_node` (day-2: join a node to the cluster)

[`scenario/add_node/main.yml`](scenario/add_node/main.yml) - join **one** new node to
an already-formed Redis cluster (`redis_type=cluster` mode). The analog of
`redis-cli --cluster add-node`, but entirely through the `community.redis.cluster`
plugin (`action: add-node`) - no `redis-cli`/shell. The new node must already be
bound to the incarnation as a Soul (onboarding is outside the scenario); targeting is
by stable SID via `where:`. The run is called against the whole incarnation: the
roster (`soulprint.hosts`) contains both existing nodes and the newcomer - endpoints
are built from it. Four steps:

1. **guard** (`core.cmd.shell`, `run_once`) - `new_node_sid` and `seed_sid` must be
   distinct members of the run's roster (keeper-side at render, based on
   `soulprint.hosts`).
2. **`apply: destiny: redis`** on the **new** node (`where: soulprint.self.sid == input.new_node_sid`)
   - install + render `redis.conf` (cluster directives from
   `incarnation.state.redis_config` - the **source of truth**, fixed by `create`, not
   recomputed → no drift) + systemd. The new node's `users.acl` merges in the
   **system users** from `essence.system_acl_users` on top of `state.redis_users` -
   without the system `replica` user, the node won't be able to `PSYNC` and the
   cluster won't accept it as a replica (see [★ Re-merging on day-2](#-re-merging-on-day-2-invariant)).
3. **health-gate PING** on the new node - it must respond BEFORE joining the cluster.
4. **add-node** (`community.redis.cluster`, `action: add-node`, `run_once`) - the
   `new_node`/`seed`/`master` endpoints are built from the roster by SID. The plugin
   sends `CLUSTER MEET` via `seed` + `REPLICATE` (for `role: replica`), or adds an
   empty master (for `role: master`; it does not move slots - that's a separate
   `reshard`, follow-up). `master` is only passed when `role: replica` and
   `master_sid` is set; otherwise the plugin picks the master itself (load
   balancing). `incarnation.state` is **not mutated** by this scenario in the current
   slice (the exact role of each SID isn't written to state - populating
   `redis_hosts` is a follow-up). `add-node` state params - in the
   [per-module doc](../../../docs/module/community/redis/README.md).

### `remove_node` (day-2: evict a node from the cluster)

[`scenario/remove_node/main.yml`](scenario/remove_node/main.yml) - evict **one** node
from an already-formed Redis cluster (`redis_type=cluster` mode). The analog of
`redis-cli --cluster del-node`, but entirely through the `community.redis.cluster`
plugin (`action: remove-node`) - no `redis-cli`/shell. The evicted node and the
`seed` must be in the run's roster (`soulprint.hosts`); targeting is by stable SID.
Two steps:

1. **guard** (`core.cmd.shell`, `run_once`) - `remove_node_sid` and `seed_sid` must
   be distinct members of the roster (keeper-side, symmetric with `add_node`).
2. **remove-node** (`community.redis.cluster`, `action: remove-node`, `run_once`) -
   the `node`/`seed` endpoints are built from the roster by SID. The plugin reads
   `CLUSTER NODES` from `seed` and branches: a master with slots → **slot
   migration** to the remaining masters (`SETSLOT`/`MIGRATE`/`SETSLOT NODE`, online -
   no data loss) + `CLUSTER FORGET` on all nodes; a replica / a master without slots
   → just `CLUSTER FORGET`. Idempotent (node already gone → no-op). Decommissioning
   the host itself (stopping redis, cleaning up `nodes.conf`, removing the Soul) is
   **outside** the scenario; `incarnation.state` is not mutated (symmetric with
   `add_node`). `remove-node` state params - in the
   [per-module doc](../../../docs/module/community/redis/README.md).

### `reshard` (day-2: move N slots between masters)

[`scenario/reshard/main.yml`](scenario/reshard/main.yml) - move **N** hash slots from
one master (`from_sid`) to another (`to_sid`) in an already-formed cluster
(`redis_type=cluster` mode). The analog of `redis-cli --cluster reshard`, entirely
through the `community.redis.cluster` plugin (`action: reshard`) - no
`redis-cli`/shell. Both masters must be in the run's roster (`soulprint.hosts`);
targeting is by stable SID. Two steps:

1. **guard** (`assert:`, keeper-side at render) - `from_sid` and `to_sid` must be
   distinct members of the roster; `false` aborts the render with a `message` BEFORE
   dispatch.
2. **reshard** (`community.redis.cluster`, `action: reshard`, `run_once`) - the
   `from`/`to` endpoints are built from the roster by SID. The plugin reads
   `CLUSTER NODES` from `from`, takes the first `slots` slots of the source in
   ascending order and migrates each one (`SETSLOT`/`MIGRATE`/`SETSLOT NODE`, online).

> **★ `reshard` is NOT idempotent** (deliberately, exec-style day-2): a repeated apply
> will move **another** `slots` slots from `from` to `to`. The operator calls it
> **explicitly**, exactly as many times as transfers are needed - this is **not** part
> of converge. Partial-failure semantics (no auto-rollback) - in the
> [per-module doc](../../../docs/module/community/redis/README.md).

### `restart` (day-2: safe rolling-restart)

[`scenario/restart/main.yml`](scenario/restart/main.yml) - rolling-restart Redis
without changing the config (sentinel/cluster modes). Each host's actual role is
**volatile** (after a possible failover, the `create`-declared role is already
stale), so the role is taken via a **live probe** (`community.redis.role`, `INFO
replication`) immediately before targeting, not from `incarnation.state` (ADR-008).
Replicas are restarted **one at a time** (`block` + `serial: 1`: a wave = {restart,
health-gate}), the master - as a **separate task after all replicas** (the
rolling invariant "master last"). Replica health-gate -
`community.redis.replica-synced` (a strict resync check `master_link_status:up`, not
just `PONG`); restarting the daemon itself - `core.service.restarted`. `state` is
unchanged - only a record in `state_history`.

> **★ Day-2 source of truth = `incarnation.state`, not `essence`** ([production-conventions §7a](../../../docs/destiny/production-conventions.md)).
> The plugin's connection TLS discriminator (whether TLS or plaintext, on which port)
> is taken from the deployed `incarnation.state.redis_config` (`'tls-port' in
> incarnation.state.redis_config`), and **not** from `essence.tls_*`: at create time
> the operator chose the channel mode via `input.connection_mode`
> (`tls`/`tls_plain`/`plain`), and that deployed configuration is only recorded in
> `state`. Looking at `essence` would give a plaintext connection against a TLS-only
> Redis (a failed health-gate). The CA secret is not stored in `state` (security) -
> it's resolved via `vault(essence.tls_ca_ref)`, only the path to the PEM on the host
> is materialized in `state`.

A live failover (`SENTINEL FAILOVER` / `CLUSTER FAILOVER` before restarting the
master) is in the backlog (a plugin `failover` verb); for now the master is restarted
directly (a brief outage during the restart). See [In progress](#in-progress).

> **★ `restart` operates at the systemd-unit level, not the config level** ([production-conventions §6a](../../../docs/destiny/production-conventions.md#6a-hot-reload-is-preferable-to-restart-for-services-with-live-reconfig)).
> Changes to `redis.conf`/`users.acl`/TLS material do **not** require a restart -
> they're applied by the day-2 hot-reload scenarios below
> (`update_config`/`add_user`/`rotate_tls`). `restart` is only needed when the
> systemd unit itself changes (the hardening drop-in) or for an explicit rolling
> restart of the daemon; in destiny [`redis/tasks/server.yml`](../../destiny/redis/tasks/server.yml)
> the reactive restart is narrowed to `onchanges: [redis_hardening]`.

### `update_config` (day-2: hot-reload `redis.conf` directives)

[`scenario/update_config/main.yml`](scenario/update_config/main.yml) - change
`redis.conf` directives on an **already running** Redis **without restarting the
process** (hot-reload via `CONFIG SET`). The operator supplies a changed subset of
the create input (`memory_mb`/`maxmemory_policy`/`persistence`/`redis_settings`); the
scenario **recomputes** the final `redis_config` with the same compute translation as
`create`, but with `incarnation.state` as the base layer (anything the operator
didn't set keeps the previously deployed value - the day-2 source of truth is
`state`, [§7a](../../../docs/destiny/production-conventions.md#7a-day-2-source-of-truth--incarnationstate)).
Two steps:

1. **re-render** `redis.conf` to disk with the new merged config (a full file - the
   desired state for the next process restart). The same destiny brick also
   re-renders `users.acl`, so the task **re-merges the system users** from
   `essence.system_acl_users` on top of `state.redis_users` - otherwise the
   hot-reload pass would wipe out the service users (see
   [★ Re-merging on day-2](#-re-merging-on-day-2-invariant)).
2. **hot-reload** (`community.redis.config`, `CONFIG SET` + `CONFIG REWRITE`): the
   **whole** `compute.redis_config` is passed, the plugin itself skips
   startup-only directives via a denylist (`port`/`dir`/`aclfile`/…) and applies only
   the hot-settable ones. Idempotent - an honest `CONFIG GET` diff in the plugin →
   a repeated run yields `changed=false`.

`validate` requires at least one changed field. TLS material and ACL are **not**
touched (separate scenarios exist for those). `state` records the new
`redis_config` + the changed namedfields. `config` state params (incl. the denylist)
- in the [per-module doc](../../../docs/module/community/redis/README.md#config--params).

### `add_user` (day-2: add an ACL user via `ACL LOAD`)

[`scenario/add_user/main.yml`](scenario/add_user/main.yml) - add (or override)
**one** ACL user on a running Redis **without a restart**. The operator supplies
**one `AclUser` entry** (`input.user`: `name` + `perms` + `state`) - the same
`$type: AclUser` type as an element of the create `input.users` array
([`types.yml`](types.yml), ADR-062). `perms` is validated by the **same re2
`AclUser` pattern** as in create (blocks day-2 garbage: an invalid ACL string is cut
off at input validation, never reaching `users.acl`). The password is **not** in the
input - it lives in Vault under the convention
`secret/redis/<incarnation>/users/<name>#password`, resolved keeper-side. Bulk
editing of the **entire** operator-extra set is a separate scenario
[`update_users`](#update_users-day-2-bulk-edit-the-set-of-acl-users)
(bulk-replace). Two steps:

1. **re-render** `users.acl` to disk with the new set: the **system** service users
   (from `essence.system_acl_users`) + operator-extra (`state.redis_users` plus the
   one being added, upsert by name). The re-render writes the **whole** file, so
   re-merging the system users is mandatory - otherwise the re-render would wipe out
   `replica`/`monitoring`/`sentinel`/`haproxy` (see
   [★ Re-merging on day-2](#-re-merging-on-day-2-invariant)). Per-user passwords come from
   Vault, the `.tmpl` writes the hash, not plaintext. `input.username` ∈ the reserved
   names is rejected by [`validate:`](scenario/add_user/main.yml) (422).
2. **hot-reload ACL** (`community.redis.acl`, `ACL LOAD`): the live instance
   re-reads the entire `aclfile`. Idempotent by construction; the plugin diffs
   `ACL LIST` before/after (`changed=false` on a match).

`state.redis_users` is mutated with the new set - a typed `AclUser` array
(`[{name, perms, state}]`, **upserted** by the `name` field, **without** the
password - security; the type is from [`types.yml`](types.yml), ADR-062). One user
per run (an atomic operation). `acl` state params - in the
[per-module doc](../../../docs/module/community/redis/README.md#acl--params).

### `update_users` (day-2: bulk edit the set of ACL users)

[`scenario/update_users/main.yml`](scenario/update_users/main.yml) - **bulk** edit of
**all** operator-added ACL users on a running Redis **without a restart**
(**bulk-replace** via `ACL LOAD`). The operator supplies the **full new set** of
operator-extra users - `input.users` (an array of `AclUser`, the same
`$type: AclUser` as create/add_user). This set **entirely replaces**
`incarnation.state.redis_users`:

- **boundary with `add_user`** - `add_user` adds/overrides **one** user (a targeted
  upsert, the set grows); `update_users` sets the **whole** set at once, so
  **removing** a user is expressed by **excluding** it from the array (a user absent
  from the new set drops out of `users.acl` and out of `state`). An empty
  `input.users: []` removes **all** operator-extra users;
- **system users are untouched** - `replica`/`monitoring`/`sentinel`/`haproxy` are
  **not** in `state.redis_users` (state only holds operator-extra), they're merged
  into `users.acl` from `essence.system_acl_users` on **every** render (see
  [★ Re-merging on day-2](#-re-merging-on-day-2-invariant)). Bulk-replacing operator-extra
  removes it; a system name in `input.users` is rejected by the `validate`-guard (422).

Two steps (like `add_user`): **re-render** the full `users.acl` (system users from essence +
operator-extra = `input.users` entirely, per-user passwords from Vault) + **hot-reload**
(`community.redis.acl`, `ACL LOAD` - the live instance re-reads `aclfile`; users removed from
the set disappear from `ACL LIST`). `state.redis_users` is mutated **entirely**
(`set: redis_users` = `input.users`, **without** the password - security). The connection's
TLS discriminator - from `state.tls` (like `add_user`/`restart`). `acl` state params - in
[per-module doc](../../../docs/module/community/redis/README.md#acl--params).

### `rotate_tls` (day-2: rotate TLS cert/key/CA without a restart)

[`scenario/rotate_tls/main.yml`](scenario/rotate_tls/main.yml) - rotate Redis's
TLS material **without a restart**: Redis 6.2+ re-reads cert/key/CA on the fly via
`CONFIG SET tls-*-file`. The operator supplies **new Vault paths** for the server
cert/key/CA (`cert_ref`/`key_ref`/`ca_ref`, each optional - partial rotation; an
unset one keeps the current value from `state.tls`). Three steps:

1. **guard** (`assert:`, keeper-side) - TLS must be enabled (`state.tls.enable=true`);
   rotation on a plaintext instance is meaningless, `false` aborts the render BEFORE dispatch.
2. **re-render the PEM** into `${conf_dir}/tls/{redis.crt,redis.key,ca.crt}` from the new
   refs (via `vault(ref)` in the `content` cell, seal masking). The step is marked
   `register: tls_certs` - an applier-register (orchestration.md §2.1.1): the engine emits
   a synthetic `core.noop.run` with an aggregate `changed = OR(child.changed)` over the
   child `core.file.present` tasks (`redis.crt`/`redis.key`/`ca.crt`).
3. **re-read under `onchanges`** - three `community.redis.command` tasks (`CONFIG SET
   tls-cert-file` / `tls-key-file` / `tls-ca-cert-file`) recreate the SSL_CTX on the live
   instance, **gated on `onchanges: [tls_certs]`**: they run only when at least one PEM
   actually changed.

> **★ Idempotent via `onchanges`.** The three `CONFIG SET tls-*-file` calls are gated on
> `onchanges: [tls_certs]` (the applier-register of step 2), **not** `changed: true`. Redis
> 6.2+ recreates the SSL_CTX on `CONFIG SET tls-*-file` even when the path is unchanged, and
> re-reads the PEM from disk - which is why **`command`** (a raw verb) is used rather than
> the `config` state: an honest-diff `community.redis.config` would consider `CONFIG SET
> tls-*-file` a no-op at the same path and **not** fire the command. The `onchanges` gate
> gives converge semantics: a new ref → a new PEM → `core.file.present` `changed` → the
> `tls_certs.changed=true` aggregate → re-read. A repeated run with the same material →
> files don't change → `tls_certs.changed=false` → the three `CONFIG SET` calls are
> **skipped** → the whole scenario is a no-op. This is "converge to the desired state," not
> "force a re-read on every call." Rendering the PEM is itself idempotent too (the same ref
> → the same file). `state.tls.*_ref` records the new refs; `enable`/`only`/`port` don't change.

### `migrate_cluster` (day-2: migrate from an external cluster)

[`scenario/migrate_cluster/main.yml`](scenario/migrate_cluster/main.yml) - **deploy a
new** Redis cluster **and seed** it with data from an **external** (old) cluster via
**native Redis replication** (`REPLICAOF` pointed at the external source). This is a
**create-like** scenario (`create: true` - stands up infrastructure from scratch, like
`create`), but it additionally connects the fresh cluster to the external source and
waits for it to catch up with the source's replication offset. The source stays
**alive** - the final detach (`REPLICAOF NO ONE`) is done by a separate day-2
[`detach_source`](#detach_source-day-2-detach-the-external-source) **after** the
operator has stopped writes to the old cluster.

**Dispatched by `redis_type`** (like `create`): `main.yml` is the dispatcher, both
branches are implemented via conditional-include:

- **sentinel** ([`migrate_cluster/sentinel.yml`](scenario/migrate_cluster/sentinel.yml)) -
  (1) `include redis-deploy-sentinel.yml` (the same shared service-level deploy body as
  `create/sentinel.yml` - stand up the new cluster; **SENTINEL MONITOR is deferred**,
  `sentinel_monitor_now: false`, see below); (2) the fresh master replicates the
  **external** source (`community.redis.replica` with `source_external: true`); (3) an
  **offset-lag gate** (`community.redis.offset-synced`, `retry`/`until`) - blocks until
  the new master catches up with the source within `lag_threshold`. After this,
  `seeded_from.detached: false` (the source is still connected - `detach_source` is
  needed);
- **cluster** ([`migrate_cluster/cluster.yml`](scenario/migrate_cluster/cluster.yml)) -
  slot-aware migration of an honest hash-slot Redis Cluster: joinable deploy of new
  nodes (`include redis-deploy-cluster-joinable.yml`) → join-external (1:1 replicas of
  the old masters) → failover-takeover (promote the new nodes to masters) →
  forget-external (evict the old nodes). **Atomic** within the migrate phase → no
  detach needed (`seeded_from.detached` is `true` right away). Operational
  preconditions (cluster-bus, matching password, graceful failover) are **on the
  operator** (the header of `migrate_cluster/cluster.yml`). A cluster→cluster
  migration with replicas on the source side is rejected (case
  `cluster-migrate-replicas-rejected`).

**★ SENTINEL MONITOR is deferred (split-brain fix).** In the sentinel migration branch
the sentinel daemons are brought up by the deploy body (step 1: `sentinel.conf` + unit +
PONG-gate), but the `SENTINEL MONITOR` command **is not issued**
(`sentinel_monitor_now: false`): while seeding is in progress, the new master is still
in the slave role, and an early MONITOR would have seen a slave-as-master and triggered
a failover → split-brain. MONITOR is executed by `detach_source` **after**
`REPLICAOF NO ONE` (the new master is a master again).

**★ External source credentials.** Service passwords follow the convention
`secret/redis/<incarnation>` (keeper-side `vault()`). **The exception** is the
**external** source's credentials: the operator supplies its Vault **path** (not the
value) via `input.source.password_ref`, under a strict pattern-guard
`secret/redis/migrate/*` (the source belongs to someone else - its secret doesn't live
under our convention). `migrate` **persists** these references in
`state.seeded_from.{source_password_ref, source_tls_ca_ref}` (v10) - so that
`detach_source` can perform the final offset-gate against the AUTH/TLS source.

**★ Boundary (out of scope): quiescing writes to the old cluster.** Soul Stack does not
manage application client traffic - stopping writes to the source before the final
`detach` is done **by the operator themselves**. Data written to the source **after**
the `detach_source` offset snapshot will be lost if the operator hasn't stopped writes.

L0 cases - [`migrate_cluster/tests/`](scenario/migrate_cluster/tests/):
[`sentinel-migrate-1master-1replica`](scenario/migrate_cluster/tests/sentinel-migrate-1master-1replica/case.yml),
[`cluster-migrate-2shards`](scenario/migrate_cluster/tests/cluster-migrate-2shards/case.yml),
[`sentinel-migrate-provision-enabled`](scenario/migrate_cluster/tests/sentinel-migrate-provision-enabled/case.yml)
(provision + migration in a single run),
[`cluster-migrate-replicas-rejected`](scenario/migrate_cluster/tests/cluster-migrate-replicas-rejected/case.yml).
Plugin state (`replica` `source_external`, `offset-synced`) - in the
[per-module doc](../../../docs/module/community/redis/README.md).

### `detach_source` (day-2: detach the external source)

[`scenario/detach_source/main.yml`](scenario/detach_source/main.yml) - **detach** the
external (old) cluster from an incarnation seeded via `migrate_cluster`. Once the new
cluster has caught up with the source's offset **and the operator has stopped writes**
to the old cluster, the scenario re-checks the offset one final time (fail-closed) and
switches the new master to standalone mode (`REPLICAOF NO ONE`) - the source is no
longer needed.

**Dispatched by `incarnation.state.redis_type`** (day-2 knows the mode from state, not
from input - `detach` has no input `redis_type`): the **sentinel** branch is implemented
([`detach_source/sentinel.yml`](scenario/detach_source/sentinel.yml)); the **cluster**
branch is a fail-closed `assert` stub
([`detach_source/cluster.yml`](scenario/detach_source/cluster.yml)): a cluster migration
is atomic within the migrate phase, so it needs no separate detach.

- **The source comes from state, not from input:** taken from
  `incarnation.state.seeded_from.source_endpoints[0]` (recorded by `migrate_cluster`) -
  the operator does **not** pass it again (this rules out "detaching the wrong source").
  `detach_source` has **no** input contract (`input: {}`) - the source, mode, and
  connection data all come from `state`.
- **Source credentials - from the refs in state (v10):** the final offset-gate connects
  to the external source; the source's password/CA are resolved from
  `state.seeded_from.{source_password_ref, source_tls_ca_ref}` via `vault()`
  keeper-side. Before v10 these refs weren't stored → the gate against an AUTH/TLS
  source failed to connect → detach never succeeded (`error_locked`); v10 closed this
  gap.
- **Guard against a non-migrated incarnation:** `detach` is a plain day-2 operation, and
  the operator can run it on a `create`d incarnation that **never migrated** (its
  `seeded_from.source_endpoints` is empty). This case is caught by an `assert`-guard in
  `sentinel.yml` with a clear "nothing to detach" failure, rather than an opaque
  index-out-of-bounds.
- **Idempotency:** detaching an already-detached incarnation
  (`seeded_from.detached == true`) - `REPLICAOF NO ONE` on an already-standalone master
  is a no-op (the `community.redis.detached` plugin state is idempotent).

L0 cases - [`detach_source/tests/`](scenario/detach_source/tests/):
[`sentinel-detach-source`](scenario/detach_source/tests/sentinel-detach-source/case.yml),
[`sentinel-detach-source-auth`](scenario/detach_source/tests/sentinel-detach-source-auth/case.yml)
(AUTH/TLS source - the v10 fix),
[`empty-seeded-source`](scenario/detach_source/tests/empty-seeded-source/case.yml)
(guard against a non-migrated incarnation).

### `destroy` (teardown cloud-provisioned VM)

[`scenario/destroy/main.yml`](scenario/destroy/main.yml) - **tear down the VMs** raised
by a create/migrate run via cloud-provision ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md)),
and cascade-clean the keeper registries for those hosts. A **lifecycle** scenario, **not**
runnable from the Run form - a terminal flow triggered by `DELETE /v1/incarnations/{name}`
(keeper treats the name `destroy` as a lifecycle phase, `TerminalDestroy` mode; symmetric
with `create` as bootstrap).

- **What to tear down is a fact from state, not input** (`input: {}`): `destroy` reads
  back `provisioned_vm_ids` (provider id for `core.cloud.destroyed`), `provisioned_sids`
  (Keeper id for cascade) and `provisioned_provider`, all recorded by the provision run.
- **Cascade in one PG transaction:** `PluginHost.Destroy(vm_ids)` + `souls → destroyed`,
  active `soul_seeds → orphaned`, active `bootstrap_tokens → burned`. The cascade is
  needed because the Reaper does **not** clean up pending-VM-souls itself - without
  `sids` the records would be left orphaned (a break of bootstrap-token anti-replay).
- **★ `sids ↔ vm_ids` pairing (assert):** both are read as a **pair** (a length mismatch
  means an orphan). `provision` writes both under one guard (a single source of size),
  so their lengths are equal in the normal case; a render-time `assert` catches state
  corruption (a manual edit / a bug) **BEFORE** an irreversible destroy. On a
  non-provisioned incarnation both sides default to `[]` → `0 == 0` → vacuously true.
- **★ Group-drop on non-provisioned incarnations:** on an incarnation deployed
  **without** provision (deployed onto pre-existing VMs), there's nothing to tear down.
  The gate `when: size(provisioned_vm_ids) > 0` on the `destroyed` task is static
  (depends only on `incarnation.*`) → a render-phase **group-drop**: with an empty
  `provisioned_vm_ids` the task never physically enters the plan. Teardown without a
  cloud call goes through cleanly.
- **★ `state_changes` are intentionally absent:** `destroy` is terminal - the
  incarnation is deleted (`auto_destroy`), so clearing `provisioned_*` in state would be
  pointless (state goes away together with the incarnation).

L0 cases - [`destroy/tests/`](scenario/destroy/tests/):
[`destroy-provisioned`](scenario/destroy/tests/destroy-provisioned/case.yml),
[`destroy-not-provisioned`](scenario/destroy/tests/destroy-not-provisioned/case.yml)
(group-drop of the `destroyed` task),
[`destroy-sids-vmids-mismatch`](scenario/destroy/tests/destroy-sids-vmids-mismatch/case.yml)
(the pairing assert-guard).

## Security

Passwords - **from Vault**, not in the scenario's input contract. The scenario reads
them keeper-side with the CEL function `vault(...)` in the render phase
(templating.md §2.3/§4), by convention:

- requirepass: `secret/redis/<incarnation.name>#password`;
- per-user (operator-extra **and** system `replica`/`monitoring`/`sentinel`/`haproxy`):
  `secret/redis/<incarnation.name>/users/<name>#password`;
- the sentinel daemon's `default` user (in `sentinel-users.acl`): the primary
  `secret/redis/<incarnation.name>#password` (shared with redis-server's requirepass).

The path is built from a trusted context (the incarnation, not operator input). What
reaches destiny and the plugin via `apply.input` / `params` is already the
**resolved value** - the password arrives on the host as a value, not a reference; the
Soul vault client is never pulled in (ADR-012). Git holds neither the value nor an
operator pointer to the secret. In `users.acl` **and** `sentinel-users.acl` the
password is written as a **hash** (`#<sha256>`) - plaintext never reaches the file.
Both files are `mode 0640`, owner/group `redis` (readable only by the service). The
`community.redis` plugin does not log `params["password"]` (ADR-010).

> **★ Exception: `sentinel auth-pass` in `sentinel.conf` - plaintext on disk.**
> The password the sentinel daemon uses to authenticate to the monitored master
> (`sentinel auth-pass <master> <pass>`) is written into `sentinel.conf` **in the
> clear** - this is a requirement of the Sentinel protocol (it does **not** accept a
> `#<sha256>` hash, unlike the ACL aclfile). The `sentinel.auth_pass` field is marked
> `secret: true` (masked in logs/traces/state), and the file itself is `mode 0640`
> owner `redis`, but the value is stored in the clear on disk. This is the only place
> where a Redis secret sits as plaintext (by necessity, by-protocol) - unlike the
> hashes in both aclfiles.

**TLS PEM - Vault PATHS, not a literal PEM.** The operator supplies `tls.cert_ref` /
`tls.key_ref` / `tls.ca_ref` - Vault **paths** (form `<mount>/<path>#<field>`,
overriding the essence defaults `essence.tls_*_ref`), **not** the PEM itself. destiny
reads the PEM with the CEL function `vault(ref)` **directly in the `content` cell** of
the `core.file.present` task (not a `.tmpl` stub, not passing an already-resolved PEM
through `apply.input`): the seal detector marks the cell as sealed by the vault layer
during destiny's render phase (ADR-010 §7.4), and masking hides the PEM in
`error_summary`/`state`. The PEM lands in the files
`/etc/redis/tls/{redis.crt,redis.key,ca.crt}` (mode `0600`, owner `redis`); the vault
client is never pulled in on Soul (ADR-012). A literal operator-supplied PEM is
deliberately unsupported: it would go through `apply.input`, and destiny's render-phase
seal doesn't see the secret-input schema there → the PEM wouldn't be masked.

## L0 run

L0 trial (Trial, ADR-023), render-only, hermetic - from the `keeper/` directory:

```sh
# sentinel single-host (standalone equivalent: 1 master + sentinel daemon)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/full-stack/case.yml
# cluster
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/cluster-create-3shards/case.yml
# sentinel
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml
# add_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/add_node/tests/add-replica-explicit-master/case.yml
# remove_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/remove_node/tests/remove-node-from-cluster/case.yml
# reshard (day-2, NOT idempotent)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/reshard/tests/reshard-slots-from-to/case.yml
# restart (day-2, rolling)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/restart/tests/rolling-restart-replicas/case.yml
# add_user (day-2, a targeted upsert of one user)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/add_user/tests/add-user-plaintext/case.yml
# update_users (day-2, bulk-replace the entire operator-extra set)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/update_users/tests/bulk-replace-removes-user/case.yml
```

[The `create/tests/full-stack` case](scenario/create/tests/full-stack/case.yml)
checks the sentinel single-host plan (`replicas_per_master: 0` - the standalone
equivalent): destiny `redis` tasks (install + render `users.acl` + render `redis.conf`
+ `sentinel.conf` + systemd hardening drop-in + running + restarted) + the
`community.redis.command` task (`PING`). There's no manual `daemon-reload` step in the
plan: `core.service` (`daemon_reload: auto`, default) reloads the systemd
configuration itself when the unit file changes, before start/restart. The slice's
main guard is that the computed `maxmemory` (`768mb`), the persistence preset, last-wins
on the conflicting `timeout`, `maxmemory-policy` from input, and the passthrough
`tcp-backlog` all correctly fold into the merged `redis.conf`, and `users.acl` is
rendered with the operator's full ACL string. Vault is mocked with `fixtures.vault` →
the plan already carries password **values** (a regression guard on the keeper-side
`vault()` resolve). destiny `redis` is resolved as a mirror of production via
`fixtures.default_destiny_source` (`file://../../destiny/{name}`, a path relative to
the service root).

Cluster cases under [`scenario/create/tests/`](scenario/create/tests/):

- [`cluster-create-3shards`](scenario/create/tests/cluster-create-3shards/case.yml)
  - `shards=3`, `replicas_per_master=0` (3 hosts): checks the cluster directives in the
  rendered `redis.conf` (`cluster-enabled`/`cluster-config-file`/
  `cluster-node-timeout`/`cluster-announce-ip`), the deterministic `nodes` MAP by SID,
  and the presence of `community.redis.cluster` (`action: create`) in the plan; the
  sentinel branch is suppressed by a placeholder-skip.
- [`cluster-create-2shards-1replica`](scenario/create/tests/cluster-create-2shards-1replica/case.yml)
  - `shards=2`, `replicas_per_master=1` (4 hosts): non-zero replicas in the `nodes` MAP;
  `cluster_node_timeout` from input (`8000`, not the default).

Sentinel cases under [`scenario/create/tests/`](scenario/create/tests/):

- [`sentinel-create-1master-2replica`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
  - `replicas_per_master=2` (3 hosts): master election (the first by SID = master),
  `community.redis.replica` (`master_addr` is host-invariant),
  `community.redis.sentinel` (`monitor.ip` is host-invariant, `quorum` auto
  `size/2+1`), `redis_sentinel` in state.
- [`sentinel-no-replicas-auto-quorum`](scenario/create/tests/sentinel-no-replicas-auto-quorum/case.yml)
  - `replicas_per_master=0` (standalone equivalent, a single host): auto-quorum
  `size/2+1`, the default `master_name` from essence.

TLS and install cases under [`scenario/create/tests/`](scenario/create/tests/):

- [`tls-enabled`](scenario/create/tests/tls-enabled/case.yml)
  - `connection_mode: tls` (TLS only): the TLS-PEM tasks are active, `tls-port`/
  `tls-cert-file`/… in the merged `redis.conf`, `port 0` (plain closed).
- [`tls-enabled-no-only`](scenario/create/tests/tls-enabled-no-only/case.yml)
  - `connection_mode: tls_plain`: the TLS port is open, the plain port remains
  (no `port 0`).
- [`tls-essence-refs`](scenario/create/tests/tls-essence-refs/case.yml)
  - `connection_mode: tls`, technical TLS parameters from essence (a non-standard
  `tls_port` 7400, refs `secret/ops/redis/tls`) - the operator only sets the channel
  mode.
- [`tls-disabled`](scenario/create/tests/tls-disabled/case.yml)
  - `connection_mode` unset (default `plain`): TLS off, the PEM tasks are
  placeholder-skipped, no TLS directives.
- [`tls-cluster`](scenario/create/tests/tls-cluster/case.yml) - `connection_mode: tls`
  + cluster: `tls-replication`/`tls-cluster yes` in the merged config.
- [`install-package`](scenario/create/tests/install-package/case.yml) -
  `install_method: package` (set explicitly - the default is now `binary`): a distro
  package with an `=version` pin.
- [`install-binary`](scenario/create/tests/install-binary/case.yml) -
  `install_method: binary` (default): **four** `core.url.fetched` tasks download the
  separate `redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` binaries from
  Nexus (path `<base_url>/<arch>/Debian/<distro_ver>/<version>/…`) + a distro
  user/group + its own systemd unit + `redis-check-aof`/`redis-check-rdb` symlinks;
  the package branch is placeholder-skipped.
- [`modules-no-checksum`](scenario/create/tests/modules-no-checksum/case.yml)
  - the full set of Redis modules: `.so` files downloaded with content-idempotency
  (no integrity verification).
- [`empty-modules-base-url`](scenario/create/tests/empty-modules-base-url/case.yml)
  - an empty `essence.modules_base_url` → vanilla redis (modules.yml group-drop, no
  `loadmodule`/`.so` fetch).
- [`with-modules-base-url`](scenario/create/tests/with-modules-base-url/case.yml)
  - a non-empty `essence.modules_base_url` on Redis < 8 → the full set of `.so` files
  is downloaded.

add_node cases under [`scenario/add_node/tests/`](scenario/add_node/tests/):

- [`add-replica-explicit-master`](scenario/add_node/tests/add-replica-explicit-master/case.yml)
  - `role=replica` with an explicit `master_sid` (`master.addr` = the specified
  host's IP).
- [`add-replica-auto-master`](scenario/add_node/tests/add-replica-auto-master/case.yml)
  - `role=replica` without `master_sid` (`master.addr` empty → the plugin balances).
- [`add-empty-master`](scenario/add_node/tests/add-empty-master/case.yml)
  - `role=master` (an empty master with no slots).
- [`guard-mismatch-same-sid`](scenario/add_node/tests/guard-mismatch-same-sid/case.yml)
  - `new_node_sid == seed_sid`: the FAIL branch of the add_node guard.

In a live Keeper, the service + destiny are resolved as git repos by ref (ADR-007/009).

## In progress

Day-2 hot-reload is implemented: `update_config` (live `CONFIG SET` deltas, → state
`config`), `add_user` (a targeted upsert of one user, → state `redis_users`,
`ACL LOAD`), `update_users` (bulk-replace of the entire operator-extra set, → state
`redis_users`, `ACL LOAD`), `rotate_tls` (→ state `command`, re-read the SSL_CTX under
`onchanges: [tls_certs]` - idempotently). Also implemented: **migration from an
external cluster** -
[`migrate_cluster`](#migrate_cluster-day-2-migrate-from-an-external-cluster) (sentinel +
cluster) + day-2 [`detach_source`](#detach_source-day-2-detach-the-external-source)
(sentinel; a cluster-detach isn't needed - the migration is atomic) - and **teardown**
[`destroy`](#destroy-teardown-cloud-provisioned-vm) (lifecycle, tears down
cloud-provisioned VMs + cascade-cleans the registries). The next batches of the
redis-consolidation epic (**not yet implemented** in this service):

- day-2 sentinel: failover (switchover) and other day-2 operations for the sentinel
  topology;
- the plugin state `community.redis.failover` (`command` / `pinged` / `role` /
  `replica-synced` / `config` / `acl` / `cluster` (create/add-node/remove-node/
  reshard) / `replica` / `sentinel` already exist);
- TLS for the sentinel daemon (`:26379`): the redis-server TLS data plane is already
  implemented (operator enum `connection_mode` ∈ `tls`/`tls_plain`/`plain`; technical
  parameters live in essence), TLS for the sentinel daemon is a follow-up;
- **cloud-provision (`input.provision`) - keeper-side implemented, live awaits C1**:
  bootstrap-token delivery is implemented keeper-side (module
  `core.bootstrap.delivered`, [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md),
  SSH delivery). The render/L0 flow is valid and passes unit tests, but a live
  "VM→redis in one pass" provisioning run awaits slice **C1** (a cloud-init CA-signed
  host key to verify the host-cert without TOFU) + live-e2e - see
  ["Cloud-provision"](#cloud-provision-create-provisions-vms-live-awaits-c1).
  Teardown of provisioned VMs - the [`destroy`](#destroy-teardown-cloud-provisioned-vm)
  scenario (implemented);
- **a version-aware `redis_settings` validator in day-2 `update_config`** - so far only
  present in `create` (see ["Directive-name validator"](#redis_settings-directive-name-validator)).

Plugin state for `community.redis` (which states are implemented) - in its per-module
doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).
