# dragonfly - DragonFly service (redis-compatible in-memory store)

DragonFly is a single-binary in-memory store, **wire-compatible with Redis**
(PING/REPLICAOF/SENTINEL/INFO work) and with **native Prometheus metrics**
(built-in endpoint). The service deploys DragonFly in **sentinel mode**
(master-replica + sentinel daemon); `replicas_per_master: 0` gives a standalone-equivalent
(a single master + sentinel daemon).

The operator sets **simple typed concepts** (memory, replicas, TLS, ACL users),
and the `create` scenario **translates** them into the `dragonfly.conf` flagfile (DF flags,
underscore form). Live runtime goes through the
[`community.redis`](../../../docs/module/community/redis/README.md) plugin - no changes
needed, DragonFly is Redis-compatible.

Division of responsibilities (ADR-009):

- **destiny [`dragonfly`](../../destiny/dragonfly/)** - per-host install + render of the
  data plane (flagfile `dragonfly.conf`, `users.acl`, TLS-PEM, systemd, host tuning).
- **destiny [`redis`](../../destiny/redis/)** in **sentinel_only** mode (`deploy_redis: false`)
  - a sentinel daemon over the DragonFly master. DragonFly **does not ship** `redis-sentinel`,
  so the daemon installs the distro package `redis-server` via the `redis` destiny. In `create` -
  **two** `apply: destiny` calls (dragonfly + redis).
- **service scenario** - translates the input into the flagfile + orchestration (order/targeting/health-gate).
- **`community.redis` plugin** - interface to the live DragonFly (PING/REPLICAOF/SENTINEL/ACL).

> **★ PILOT scope.** Only **sentinel** mode. `cluster` is out of scope (DragonFly cluster is
> emulated, a separate slice). No persistence presets (DF persistence - `snapshot_cron`,
> deferred), no redis modules (`.so`), no `maxmemory_policy` (see
> ["Translation"](#translating-simple-input-into-df_config)).

## state_schema

[`service.yml → state_schema`](service.yml), `state_schema_version: 6`. Forward-only migration
chain (ADR-019):

- [`001_to_002`](migrations/001_to_002.yml) - `install` + host layout (`conf_dir`/`data_dir`)
  moved out of state into `essence` (a read-model with no readers / day-2 operations read essence
  directly). DragonFly has **no** `modules_base_url` (redis modules don't apply to DF) - three
  fields are dropped, not four;
- [`002_to_003`](migrations/002_to_003.yml) - cloud-provision read-model: `provisioned_vm_ids`
  (provider IDs of created VMs) + `provisioned_provider`;
- [`003_to_004`](migrations/003_to_004.yml) - cascade-destroy read-model: `provisioned_sids`
  (Keeper-side SID/FQDN of created VMs for teardown);
- [`004_to_005`](migrations/004_to_005.yml) - **mandatory monitoring** of the data plane
  (Slice II, [ADR-024](../../../docs/adr/0024-observability.md)): read-model `monitoring`
  (version/listen node_exporter). Existing v4 records get a conservative default (version `''`, port `:9100`);
- [`005_to_006`](migrations/005_to_006.yml) - **mandatory log shipping** of the data plane
  (Slice V-I, [ADR-067](../../../docs/adr/0067-vector-log-shipping.md)): read-model `logging`
  (version/sink/vector sources). `sink_auth_ref` is **not** written here (secret stays in Vault).
  Existing v5 records get a default (version `''`, sink `console`).

`incarnation.state` fixes what has been deployed (`required: [df_config]`):

| field | type | meaning |
|---|---|---|
| `df_version` | string | effective DragonFly version (distro pin with `package`, upstream semver with `binary`) |
| `df_config` | object | **translation result** - merged flagfile map `dragonfly.conf` (keys are DF flags, underscore form) |
| `tls` | object `{enable, cert_ref, key_ref, ca_ref}` | TLS intent + Vault **paths** to the PEM (not the PEM itself). ★ No `only`/`port`: DF terminates TLS on the **main** port |
| `memory_mb` | integer | memory budget for DragonFly, MB (from `input.memory_mb`; not set -> 0) |
| `sysctl_settings` | map string→string | applied kernel parameters (host tuning, from essence) |
| `monitoring` | object `{node_exporter_version, node_exporter_listen}` | **(v5)** read-model of the deployed node_exporter (Slice II). ★ No redis_exporter - DF exposes metrics natively |
| `logging` | object `{vector_version, vector_sink_type, vector_sink_endpoint, vector_log_sources}` | **(v6)** read-model of the deployed vector (Slice V-I). ★ No `sink_auth_ref` - secret stays in Vault |
| `replicas` | integer | replicas per master (from `input.replicas_per_master`) |
| `sentinel_quorum` | integer | always `0` (auto `size/2+1` is computed in apply, not materialized in state) |
| `df_users` | array `AclUser` | operator-extra ACL users (`[{name, perms, state}]`, [`types.yml`](types.yml), ADR-062). Passwords are **not** in state (Vault). System service accounts are **not** written here |
| `df_hosts` | array `{sid, role}` | topology hosts (written as `[]`; exact roles are a follow-up) |
| `df_sentinel` | object `{master_name, master_ip, quorum, down_after_ms, failover_timeout_ms}` | sentinel-mode facts. `master_ip` is not written by create (host-variant), `quorum` is `0` (auto) |
| `provisioned_vm_ids` / `provisioned_provider` / `provisioned_sids` | array / string / array | **(v3/v4)** cloud-provision read-model (see ["Cloud-provision"](#cloud-provision)) |

## Operator input contract

[`scenario/create/main.yml → input:`](scenario/create/main.yml) - strictly typed
structural input (Named Dict):

| field | type | meaning |
|---|---|---|
| `install_method` | enum `package`/`binary`, default `package` | `package` - distro deb `dragonfly` (`=version` pin); `binary` - upstream tarball (URL/version from `essence.binary_base_url`/`binary_version`, filename `dragonfly-<arch>.tar.gz` built by destiny from `soulprint.self.os.arch`) |
| `version` | string, `required_when` `install_method==package` | distro-native pin of the `dragonfly` package (`^([0-9]+:)?[0-9]…`). Not used with `binary` |
| `memory_mb` | integer, optional, min `64` | memory budget for DragonFly, MB; `maxmemory` is a share of it. The `min 64` floor isn't arbitrary: too small a value would truncate to `"0mb"` (unlimited) |
| `replicas_per_master` | integer, optional, default `0`, min `0` | replicas per master. roster = `1 + replicas_per_master` (sentinel daemon on every host). `0` - standalone-equivalent |
| `sentinel_down_after_ms` | integer, optional, default `5000` | how many ms the master can stay silent before sentinel considers it down (`sentinel down-after-milliseconds`) |
| `sentinel_failover_timeout_ms` | integer, optional, default `60000` | timeout for a single failover attempt (`sentinel failover-timeout`) |
| `users` | array `AclUser` | operator-extra ACL users (`[{name, perms, state}]`, [`types.yml`](types.yml), ADR-062). `perms` is a full Redis ACL string (DragonFly accepts it), validated with a re2 pattern. The name **cannot** collide with a system account (see ["System ACL users"](#system-acl-users)) |
| `df_settings` | object (passthrough, key→value strings) | arbitrary DF flags for `dragonfly.conf` (underscore form) layered on top of defaults/computed values (last-wins). ★ PILOT: names are **not** validated against a catalog - a typo shows up when DF starts, not at render time |
| `tls_enabled` | boolean, optional, default `false` | enable TLS for DragonFly. ★ TLS is placed on the **main** port 6379 (flag `--tls`; DF has no separate TLS port -> no `tls_keep_plain`/"TLS-only"). Vault paths for the PEM come from `essence.tls_*_ref` |
| `provision` | object, optional, **default-on** `{enabled: true}` | bring up VMs for the topology within the same create run (see ["Cloud-provision"](#cloud-provision)) |

**What is not in the input contract** (essence parameters or auto-computed):

- `maxmemory_policy` - DragonFly has **no** such flag (the name is a derived `INFO`/`CONFIG`
  field from the bool `--cache_mode`). DF doesn't translate per-policy redis names (`lru`/`lfu`/…) -
  eviction is out of PILOT scope;
- `sentinel_quorum` - **auto** `size(hosts)/2+1` (computed in apply);
- `sentinel_master_name` - `essence.sentinel_master_name` (default `master`);
- `conf_dir` / `data_dir` / `run_dir` - `essence` (author context, host layout);
- Vault paths for TLS (`tls_cert_ref`/`tls_key_ref`/`tls_ca_ref`) - `essence`;
- `binary_base_url` / `binary_version` - `essence` (when `install_method: binary`).

**Cross-field invariant** ([`create/main.yml → validate:`](scenario/create/main.yml),
input-only, 422 before applying): the name `input.users` must **not** be one of
`[default_admin, replica, monitoring, sentinel, haproxy]` - operator-extra entries cannot take
a system account's name (an essence top-up would silently overwrite it).

## Translating simple input into df_config

`compute.df_config` merges layers via `merge()` (SHALLOW last-wins, left to right;
[templating §2.3](../../../docs/templating.md)):

1. `essence.df_config` - author defaults (`maxmemory: "256mb"`, `maxclients: 10000`);
2. computed `maxmemory` (has-guard) - `memory_mb * memory_reserve_percent / 100` (MB,
   25% reserved for the OS); e.g. `memory_mb: 1024` -> `maxmemory: "768mb"`;
3. `input.df_settings` - operator passthrough;
4. TLS block (when `tls_enabled`) - DF flags `tls`/`tls_cert_file`/`tls_key_file`/
   `tls_ca_cert_file`/`tls_replication` (underscore form, values are PEM paths under `${conf_dir}/tls/`).

**No** persistence preset (deferred), **no** cluster directives (sentinel-only), **no**
`maxmemory_policy` (DF has no such flag - `absl` FATALs on an unknown one). The merge base and
data tables are in [`essence/_default.yaml`](essence/_default.yaml).

## Host-tuning extras

Mode-agnostic per-host additions - an **unconditional baseline** (not an operator choice):
disabling Transparent Huge Pages (drop-in `disable-thp.service`), logrotate
(`/var/log/dragonfly/*.log`), sysctl (`core.sysctl.applied` -> `/etc/sysctl.d/30-dragonfly.conf`).
The sysctl parameter set is reused from the `redis` service (same in-memory-store recommendations:
overcommit/swappiness/network buffers/backlogs), data table -
[`essence/_default.yaml → sysctl_settings`](essence/_default.yaml).

## System ACL users

Besides operator-extra accounts (`input.users`), the service **always** tops up system ACL users:
`default_admin` (full permissions `~* &* +@all`), `replica` (PSYNC replication), `monitoring`
(metrics), `sentinel` (AUTH sentinel↔df), `haproxy` (health-check). perms live in
[`essence/_default.yaml`](essence/_default.yaml) as two sets -> **two** aclfiles: `users.acl`
(DragonFly, `system_acl_users`) and `sentinel-users.acl` (sentinel daemon, `system_acl_users_sentinel`).

**★ `default_admin` redesign** (symmetry with redis). `requirepass` was removed from the
flagfile; **all** intra-cluster authentication (replication `masterauth`, sentinel monitor/auth,
health-PING) goes through the system `default_admin` account. The `community.redis` plugin
connects as `username=default_admin`. The built-in DragonFly `default` user is rendered `off`
(absent from the sets) until the operator declares it in `input.users`.

In every task that renders `users.acl` (create + day-2 `add_user`/`update_users`), the set
is assembled by a double `merge()`: system accounts from essence (bottom layer) + operator-extra
(on top, last-wins). System accounts are **not stored in state** - they are re-added from essence
on **every** render, otherwise a re-render would wipe `replica`/`sentinel` and break day-2 replication.

## Scenarios

### `create` (sentinel mode, inline body)

[`scenario/create/main.yml`](scenario/create/main.yml) - a single mode, no dispatcher; the run
body is inline. Steps:

1. **generate-if-absent** (`core.vault.kv-present`, `on: keeper`) - create itself generates
   missing passwords cryptographically at random (32 alphanumeric) for all system + operator-extra
   accounts. Ordering invariant: the write to Vault happens **before** the render phase of tasks
   that read the same secrets via `${ vault(...) }` (ADR-056);
2. **cloud-provision** (conditional, when `provision.enabled` - see below);
3. **size-guard** - render-time `assert: size(soulprint.hosts) == 1 + replicas_per_master`
   (keeper-side, aborts the render before install; `validate` won't work - it needs `soulprint.hosts`);
4. **`apply: destiny: dragonfly`** - install + render `dragonfly.conf`/`users.acl` + systemd.
   `masteruser`/`masterauth` are added to the flagfile as regular flags (replica→master AUTH must
   persist across a restart - `CONFIG SET` isn't persisted);
5. **health-gate PING** (`community.redis.command`) - over the **UNIX socket** `compute.local_addr`
   (`unix:${run_dir}/dragonfly.sock`): DF with `--bind=primary_ip` does **not** listen on loopback,
   so local calls go over the socket;
6. **`apply: destiny: redis` (sentinel_only)** - sentinel daemon (`deploy_redis: false`) over the
   DragonFly master; `version` is the distro pin of the `redis-server` package
   (`essence.sentinel_redis_package_version`);
7. **REPLICAOF** (`community.redis.replica`, `where:` excludes the master by SID) - replicas
   follow the elected master (`soulprint.hosts[0]`);
8. **SENTINEL MONITOR** (`community.redis.sentinel`, on every host);
9. **health-gate PONG** on `:26379` (same-task `register.self`, ADR-056);
10. **node-exporter** and **vector** - mandatory monitoring/log-shipping (see
    ["Observability"](#observability)).

### Cloud-provision

**Default-on** ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md),
Option A): `input.provision` defaults to `{enabled: true}` - a single create run brings up VMs
for the topology **and** deploys DragonFly. The shared body is
[`scenario/dragonfly-provision.yml`](scenario/dragonfly-provision.yml): (a) cloud-create
(`core.cloud.created`, `on: keeper`; the VM count is derived from the topology
`1 + replicas_per_master`, there's no separate `node_count`), (b) delivering a per-VM bootstrap
token over SSH (`core.bootstrap.delivered`,
[ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)), (c) a blocking wait for onboarding
(`core.soul.registered` `await_online` + `refresh_soulprint`) -> the roster is re-resolved, so
size-guard/deploy see the newly created hosts. `provider`/`profile`/timeouts fall back to
`essence.provision_*`; the section is hidden from the Run form. To roll out onto an
**already-provisioned** roster - set `provision: {enabled: false}` explicitly.

### Day-2 scenarios

- **[`add_user`](scenario/add_user/main.yml)** - add/override **one** ACL user
  without a restart (hot-reload `ACL LOAD` via `community.redis.acl`). Merges into `state.df_users`,
  renders the full `users.acl`;
- **[`update_users`](scenario/update_users/main.yml)** - **bulk-replace** of the entire
  operator-extra set (a user missing from the new array is removed). System accounts are
  untouched (re-added from essence);
- **[`restart`](scenario/restart/main.yml)** - rolling-restart with no config change. Each
  host's role is taken from a live probe (`community.redis.role`), replicas one at a time
  (`serial: 1`), master last. Only the DragonFly data plane restarts (`core.service.restarted`
  of the `dragonfly` unit); the sentinel daemon is untouched;
- **[`rotate_tls`](scenario/rotate_tls/main.yml)** - cert/key/CA rotation without a restart. The
  destiny re-renders the PEMs from the new Vault refs -> three `CONFIG SET` calls -
  **`tls_cert_file`/`tls_key_file`/`tls_ca_cert_file`** (★ underscore form - a DragonFly
  specificity, not the hyphenated form redis uses) under `onchanges`. Precondition: TLS must
  already be enabled (`assert` `state.tls.enable`);
- **[`destroy`](scenario/destroy/main.yml)** - teardown (a terminal `DELETE` flow, not runnable).
  Two branches: **cloud-cascade** (when `provisioned_vm_ids > 0` - `core.cloud.destroyed` + cascade
  of the souls/seeds/tokens registries) and **Soul-side** (stop services + remove `/etc` artifacts
  per `install_method`).

## Observability

Both surfaces are a **mandatory invariant of the data service**, not an operator choice: `create`
deploys them **unconditionally** (no `when` gate) on **every** host at the end of the run,
composed from reusable standalone destinies via `apply: destiny` (isolated render,
ADR-009). Versions/ports are author context in `essence` (the operator can override in
`spec.essence`); destiny derives `arch` from `soulprint.self.os.arch`.

### node-exporter (host metrics, pull - Slice II)

Step 6 of the deploy (**after** the deploy branch) unconditionally installs
[`node-exporter`](../../destiny/node-exporter/) ([ADR-024](../../../docs/adr/0024-observability.md)).
essence: `node_exporter_version` (`1.8.2`), `node_exporter_listen` (`:9100`),
`node_exporter_base_url` (an internal Nexus raw-proxy), `node_exporter_allow_private` (`true` - SSRF-guard
opt-out for private-resolve mirrors). Read-model - `state.monitoring`
(`node_exporter_version`/`node_exporter_listen`), mirroring `apply.input` of the node-exporter destiny.

> **★ node-exporter-ONLY (by design).** DragonFly exposes data-plane metrics **natively**
> (a built-in Prometheus endpoint) -> the `redis_exporter` counterpart is **not** installed
> (unlike the `redis` service, where redis_exporter is mandatory).

### vector (log shipping, push - Slice V-I)

Step 7 of the deploy (**after** the exporter) unconditionally installs
[`vector`](../../destiny/vector/) ([ADR-067](../../../docs/adr/0067-vector-log-shipping.md)) -
the log-shipping agent, adding a log plane alongside the metrics plane. essence: `vector_version`
(`0.40.0`), `vector_sha256` (★ placeholder - the real operator **must** supply the checksum
for the `(version, arch)` pair in `spec.essence`, otherwise it fails closed), `vector_base_url`
(an internal Nexus), `vector_allow_private` (`true`), `vector_sink_type`/`vector_sink_endpoint`/
`vector_sink_auth_ref`, `vector_log_sources`. Read-model - `state.logging`
(`vector_version`/`vector_sink_type`/`vector_sink_endpoint`/`vector_log_sources`) **without**
`sink_auth_ref` (the secret is resolved Soul-side and never lands in state - like `tls.*_ref`).

**Two log planes** (`vector_log_sources`):

- `/var/log/dragonfly/*.log` - DragonFly itself (glog, `dragonfly` destiny, `--log_dir`);
- `/var/log/redis/*.log` - the sentinel daemon (distro `redis-server`, `redis` destiny).

**Sink - Option A** (essence per-incarnation): default `sink_type: console` (safe, no
external infra needed), `sink_endpoint`/`sink_auth_ref` empty - the operator sets a real
collector (loki/elasticsearch/vector) in `spec.essence`. Agent naming follows **Slice V-I vector /
[naming-rules.md](../../../docs/naming-rules.md) §15** (upstream product name Vector.dev, like
`node-exporter`).

## Security

Passwords come **from Vault**, not from the input contract. The scenario reads them keeper-side
with the CEL function `vault(...)` in the render phase under a **single** convention (all system
accounts, including `default_admin`, **and** operator-extra):

```
secret/dragonfly/<incarnation.name>/users/<name>#password
```

There is **no** master `requirepass` secret (the `default_admin` redesign). The path is built
from a trusted context (incarnation, not operator input); the destiny and the plugin receive
only the **already-resolved value** via `apply.input`/`params` - the Soul-side vault client is
never pulled in (ADR-012). In `users.acl` the password is written as a **hash** (`#<sha256>`).
TLS-PEM - Vault **paths** (`tls_*_ref`), the destiny reads the PEM via `vault(ref)` directly in
the `content` cell (seal-masking), and the PEM ends up at
`${conf_dir}/tls/{dragonfly.crt,dragonfly.key,ca.crt}`.

## L0 run

L0 trial (Trial, render-only, hermetic) - from the `keeper/` directory:

```sh
# sentinel single-host (standalone-equivalent: 1 master + sentinel daemon)
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/standalone-no-replicas/case.yml
# sentinel 1 master + 1 replica
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/create-sentinel-1master-1replica/case.yml
# TLS on the main port
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/create-sentinel-tls/case.yml
# mandatory monitoring (node-exporter + vector in the plan)
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/monitoring-observability/case.yml
# cloud-provision on/off
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/provision-enabled-sentinel/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/provision-disabled/case.yml
# day-2
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/add_user/tests/add-user-plaintext/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/update_users/tests/bulk-replace-removes-user/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/restart/tests/rolling-restart-replicas/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/rotate_tls/tests/rotate-cert-key-ca/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/destroy/tests/teardown-package/case.yml
```

The [`monitoring-observability`](scenario/create/tests/monitoring-observability/case.yml) case
checks that the plan includes the unconditional `apply: destiny node-exporter` and
`apply: destiny vector` (steps 6/7) with the versions/sources from essence, and that the
`monitoring`/`logging` read-model made it into `state_changes`.
