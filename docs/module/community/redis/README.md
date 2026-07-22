# community.redis

MAIN interface to **live Redis** in redis consolidation (role-based concept): the service scenario orchestrates the order/targeting/rolling, and the plugin
performs **one** operation on one Redis instance. Custom plugin
`kind: soul_module` (namespace `community`, name `redis`), binary
`soul-mod-community-redis`. Implementation -
[`examples/module/soul-mod-community-redis/`](../../../../examples/module/soul-mod-community-redis/)
(`impl.go` - dispatcher + states command/config/acl, `probe.go` - read-probe states
pinged/role/replica-synced/offset-synced, `cluster.go` + `migrate.go` — cluster-state
(create/add-node/remove-node/reshard + join-external/failover-takeover/forget-external),
`replica.go` - replica-state (REPLICAOF, enable external source `source_external`),
`detach.go` — detached-state (REPLICAOF NO ONE, promotion),
`sentinel.go` — sentinel-state (MONITOR/SET reconcile), `helpers.go` —
structpb/secret-helpers).

Backend — [`github.com/redis/go-redis/v9`](https://github.com/redis/go-redis):
the plugin itself connects to Redis via TCP (`host:port`) or a unix socket
(`unix:/path`). Correspondence to core modules (`core.pkg`/`core.file`/`core.service`/
`core.sysctl`) - for everything that is NOT redis-specific (installation, render
`redis.conf`, systemd); Redis runtime itself is this plugin.

## Without dry-run preview (consciously)

The plugin **doesn't** implement `PlanReadSafe`
([ADR-031](../../../adr/0031-scry-drift.md)) and `ErrandReadSafe`
([ADR-033](../../../adr/0033-errand.md)): it remains on
`module.BaseModule`. This is a conscious choice (user decision 2026-06-22):
on `dry_run` host (Soul) applies **default-deny** - the task gets honest
"preview not supported" rather than the false "no drift". Better an obvious refusal than
quiet clean from no-op Plan.

## States

Current slice is **twelve** state (`manifest.yaml::spec.states`): three declarative
over one instance (`config`/`acl`/`replica`/`detached`), four read-probes
(`pinged`/`role`/`replica-synced`/`offset-synced`), imperative `command`,
multi-node `cluster` and `sentinel`.

| State | Destination | `changed` |
|---|---|---|
| `command` | Raw command for Redis (imperative verb-state, precedent `core.cmd.shell`/`core.exec.run`/`core.http.probe`). | `false` default; `changed: true` in params - for really mutating commands. |
| `pinged` | Health-probe via go-redis `PING` (expecting `PONG`). Read-only. Replaces idiom `command args:[PING]` - health-gate in scripts (`retry`/`until`/`failed_when` by `register.self.result`). | `false` **constructive** (probe, not change). |
| `role` | Role-probe via go-redis `INFO replication` - actual **volatile** role of the instance. Read-only. Used for `where` rolling-restart targeting (ADR-008: the role is volatile, measured by a live probe before targeting). | `false` **constructive** (probe). |
| `replica-synced` | Probe the replica gum via go-redis `INFO replication` (`master_link_status == "up"`). Read-only. Only for **replica** (master does not have this field → `synced=false`). Health-gate rolling-restart — wait until the replica is completely reset before restarting the next one. | `false` **constructive** (probe). |
| `offset-synced` | Safety-gate migration from an **external** source: checks `slave_repl_offset` of its instance with `master_repl_offset` of the external master (second connection to `source_addr`). Read-only. "Link is alive ≠ data caught up"; `caught_up=true` only with `link up` + no full-sync running + `lag <= lag_threshold`. | `false` **constructive** (probe). |
| `config` | Apply map directives `redis.conf` through `CONFIG SET` (+ optional `CONFIG REWRITE`). Startup-only directives (`port`/`dir`/`aclfile`/… - denilista) **are skipped** (CONFIG SET rejects them). | `true` with ≥1 directive applied. |
| `acl` | Hot-reload ACL of live Redis via `ACL LOAD` (re-read `aclfile` in its entirety - `users.acl` renders destiny BEFORE this step). Idempotent **by design**; the output `ACL LIST` is not** included in Output (it may carry a password-hash). | `true`/`false` by diff `ACL LIST` before/after `LOAD` (matched → `false`, no-op). |
| `cluster` | Management of a hash-slot cluster (16384 slots) via `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE`/`FORGET`/`SETSLOT`/`MIGRATE`/`FAILOVER`. Implemented `action: create` (build from scratch), `add-node` (attachment of day-2 node), `remove-node` (output of day-2 node, with migration of master slots), `reshard` (transfer of N slots master→master, day-2) and three steps of live migration between clusters: `join-external` (add new nodes with replicas of the old cluster 1:1), `failover-takeover` (promotion of new replicas to the master through graceful failover), `forget-external` (throw out old nodes). | `create`/`add-node`/`remove-node`/`join-external`/`failover-takeover`/`forget-external` are idempotent: `true` on change, `false` (no-op) on converged input. **`reshard` NOT idempotent** (see below): `true` on successful migration, `failed` on input error; There is no no-op branch. |
| `replica` | Link the instance to the master via `REPLICAOF` (+ `CONFIG SET masterauth`). Opt. `source_external: true` - binding to the **external** master (migration) with separate details `master_*`. | `true` when configuring; `false` (no-op), if there is already a replica of the desired master or `addr == master_addr` (the master itself, guard is disabled with `source_external`). |
| `detached` | Detach the instance from the master via `REPLICAOF NO ONE`, promoting it to an independent master. The final step of migration from an external source (after `offset-synced` confirmed catch-up). Idempotent: already master → no-op. | `true` during promotion; `false` (no-op), if the instance is already master. |
| `sentinel` | Reconcile Redis Sentinel (`SENTINEL MONITOR`/`REMOVE`/`SET`/`CONFIG SET`). Reconcile algorithm: classify config → monitor/set diff. | `true` when changing monitor/parameters; `false` (no-op), if everything matches. |

## command — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path` (unix socket). |
| `args` | list | required | Command with array of arguments **without shell**: `["CONFIG","SET","maxmemory","256mb"]`. The first element is verb. |
| `password` | string (secret) | optional | Redis password. vault-ref in operator-input, keeper resolves to Apply (see "Password"). Masked. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | The database number (`SELECT`) before the command. |
| `changed` | bool | optional (default `false`) | Mark the result `changed=true` (default probe semantics). |

## pinged — params

Health-probe via go-redis `PING` (expecting `PONG`). **Read-only**, `changed=false`
constructive. Replaces idiom `command args:[PING]`: the server response is placed in the same
`Output.result`, so `register.self.result == 'PONG'` in health-gate
(`retry`/`until`/`failed_when`) works without edits. Used as health-gate
before linking replicas / building a cluster / configuring sentinel. Error `PING`
(`LOADING`/`MASTERDOWN`/…) → `failed` (server response, no secret).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref in operator-input, keeper resolves to Apply (see "Password"). Masked; `PING` is not passed to arguments. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `PING`. |

**Output**: `result` - server response (`PONG`).

## role — params

Role-probe via go-redis `INFO replication` - actual **volatile** role
instance. **Read-only**, `changed=false` constructive. Replaces shell-idiom
`redis-cli role | head -1 | tr -d '\n'`: `Output.role` carries `master`/`slave` (same
values that `redis-cli role` gave). Used for `where` targeting
rolling-restart(`register.self.role == 'master'`/`'slave'`); role is volatile
(ADR-008), taken live probe before targeting, **not** from `incarnation.state`.
`INFO replication` without `role` field (truncated INFO / broken instance) → `failed`
(and not an empty role, quietly targeting no one).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref, keeper resolves to Apply (see "Password"). Masked. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `INFO`. |

**Output**: `role` - actual role of the instance, `master` or `slave`.

## replica-synced — params

Probe replica resin via go-redis `INFO replication`: checks
`master_link_status == "up"` (the replica **has caught up** with the master after the restart).
**Read-only**, `changed=false` constructive. Stricter `pinged` (`PONG` means only
that the demon is alive, but the replica might not have caught up with the master yet). `Output.synced` (bool) —
condition for health-gate (`until: register.self.synced == true`);
`Output.master_link_status` (string) - for diagnostics.

> **★ Slave path only.** The `master_link_status` field is present in `INFO
> replication` exists **only** on a replica (`role:slave`) - the master does not have it. State
> is for the rolling-restart slave path (`block.where` slave). If there is no field
> (instance - master or non-standard INFO) → `synced=false` with an explicit reason in
> `Message` (**not** silent success): otherwise health-gate replicas would silently pass through
> instance that is not yet a replica.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref, keeper resolves to Apply (see "Password"). Masked. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `INFO`. |

**Output**: `synced` (bool) — `true` ⇔ `master_link_status == "up"`;
`master_link_status` (string) — raw link status for diagnostics (`""`, if the fields
no - the instance is not a replica).

## offset-synced — params

Safety-gate migrating a replica from an **external** source via go-redis. Stricter
`replica-synced`: "link is alive ≠ data caught up." Checks `slave_repl_offset` **his**
instance (`addr`) with `master_repl_offset` **external** master (**second** connection to
`source_addr` with `source_*` details - it is the authoritative "head" for calculating lag).
**Read-only**, `changed=false` constructive. `Output.caught_up` (bool) - condition
health-gate (`until: register.self.caught_up == true`) of the final `detached` step
migrations. Used after `replica source_external` (see migration contract
["source_external" below](#replica--params-source_external)).

`caught_up=true` **only** when simultaneously: `master_link_status == "up"` +
`master_sync_in_progress == 0` (no full-sync running) + `lag_bytes <= lag_threshold`.
Without both offsets (own `addr` is not a replica or `source_addr` is not master) - `lag`
undefined → `caught_up=false` (abnormal input, not silent success).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Address of **your** instance (replica): `host:port` or `unix:/path`. |
| `source_addr` | string | required | The address of the **external** master source `host:port` is the second connection (source `master_repl_offset`). |
| `password` | string (secret) | optional | Password for **your** instance. vault-ref, keeper resolves to Apply. Masked. |
| `source_password` | string (secret) | optional | Password of the **external** source for the second connection. vault-ref, keeper resolves to Apply. Masked. |
| `lag_threshold` | int | optional (default `0`) | Allowable backlog (`master_repl_offset − slave_repl_offset`) in **bytes** for `caught_up=true`. `0` - strict full catch-up. |
| `skip_checksum` | bool | optional (default `false`) | Skip opt. reconciliation `DBSIZE` of both instances. By default, `DBSIZE` of the source and replica are placed in Output as an auxiliary signal (`caught_up` is **not** affected - offset authority). |
| `tls` / `tls_ca` | — | optional | TLS connection to **your** instance (only `tls` + `tls_ca`; this state does not have an mTLS pair). |
| `source_tls` / `source_tls_ca` | — | optional | TLS connection to an **external** source (second connection). `source_tls_ca` - PEM CA source (secret). |

**Output**: `caught_up` (bool) — final catch-up condition; `lag_bytes` (int64) —
`master_repl_offset − slave_repl_offset` (negative clamped to `0` -
read-after-write-window, non-negative lag); `master_sync_in_progress` (bool) - running
whether full-sync; with `!skip_checksum` additionally `dbsize_source` / `dbsize_replica`
(rough sanity check of sizes - different DBSIZE on the fly is normal due to TTL/eviction, on
`caught_up` has no effect).

## config — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address (same as `command`). |
| `config` | map | required | Directives `redis.conf`: `{ maxmemory: "256mb", maxmemory-policy: allkeys-lru }`. Each (except startup-only - see below) → `CONFIG SET <key> <value>`. They are applied in a deterministic order (by key). Numeric values ​​are stringified (`20000`, not `20000.000000`). |
| `password` | string (secret) | optional | See "Password". |
| `username` | string | optional | ACL-username for `AUTH`. |
| `rewrite` | bool | optional (default `false`) | After `CONFIG SET` execute `CONFIG REWRITE` (persist in `redis.conf`). |

**Honest diff (idempotency).** Before each `CONFIG SET <key>` the plugin does
`CONFIG GET <key>` and sends `SET` **only** if there is a real discrepancy in the value.
Matched for everyone → `changed=false` (no-op). This gives the repeater `update_config`
idempotency on the plugin side.

> **★ Startup-only directives are skipped (denilist).** Part of the `redis.conf` directives
> is set **only at the start of the process** - `CONFIG SET` rejects them ("can't set ...
> at runtime" / "Unknown option"). Day-2 `update_config` renders **full** `redis.conf`
> (including such directives - they are needed the next time the process is restarted) and sends
> plugin **all** `config`-map; the plugin **passes such keys** (does not fall on them),
> hot-settable applies as usual. Changing the startup-only directive will take effect when
> **next restart** of the process (it is triggered by a change in hardening unit, destiny
> [`redis/tasks/server.yml`](../../../../examples/destiny/redis/tasks/server.yml)).
> Deniliste (`startupOnlyDirectives` in [`impl.go`](../../../../examples/module/soul-mod-community-redis/impl.go)):
> `port` · `tls-port` · `bind` · `unixsocket` · `unixsocketperm` · `io-threads` ·
> `io-threads-do-reads` · `cluster-enabled` · `cluster-config-file` · `aclfile` ·
> `logfile` · `pidfile` · `dir` · `daemonize` · `supervised` · `dbfilename` ·
> `loadmodule` · `syslog-enabled` · `syslog-ident` · `syslog-facility` · `databases` ·
> `always-show-logo` · `set-proc-title` · `locale-collate` · `socket-mark-id`.

**Output**: `applied` (CSV of applied directives) `count` (number of applied)
`rewrite` (whether `CONFIG REWRITE` was executed) · `skipped` (CSV of skipped startup-only) ·
`skippedCount` (their number is for audit purposes). The values of the directives go to Output (this is the config
redis, no secret); error-path is still sanitized by `redactError` value
(the directive could come from Vault, eg `requirepass`).

## acl — params

Hot-reload ACL of live Redis: `ACL LOAD` causes the instance to re-read `aclfile`
**entirely**. `users.acl` renders destiny `redis` **before** this step (via
`core.file.rendered`, the plaintext password is not written - `.tmpl` hashes) - `acl` only
causes Redis to re-read the finished file. **Idempotent by design**: `ACL LOAD`
brings the live instance to the declared file, regardless of the current state.

`changed`-semantics: `ACL LOAD` itself does not report "changed", so the plugin does
cheap honest diff - `ACL LIST` **before and after** `LOAD` (typed path, string
per user; the order is significant). Matched → `changed=false` (live instance already
matched the file, no-op - symmetry with `config`/`cluster`/`sentinel`); different →
`changed=true`.

Params - **connection only** (`addr` + optional `auth`/`db`/TLS), like read-probe:
no acl-specific fields (the file is the source of truth, it is rendered by destiny).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref, keeper resolves to Apply (see "Password"). Masked; **not** is passed into the arguments of ACL commands (only goes into the connection). |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `ACL LOAD`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

**Output**: `users` - number of ACL users **after** `LOAD`. The output itself is `ACL LIST`
(user rules) **NOT** gets into Output (IB): the user line can
carry password-hash (`>hash` / `#sha256`). `ACL LOAD` fails when `aclfile` /
unconfigured `aclfile` - this is the Redis response (not an operator secret), goes to
`Message` as `failed`. Without dry-run preview (the plugin does not implement `PlanReadSafe`).

## cluster — params

Manages the Redis cluster **entirely via go-redis** (no `redis-cli`/shell).
The operation is selected by the `action` field. Implemented:

- day-1/day-2 over **your** cluster: `create` (build from scratch), `add-node`
(attach one node), `remove-node` (withdraw one node), `reshard` (transfer
N slots master→master);
- three steps **live migration between clusters** (old → new, without downtime):
`join-external` (infuse new cluster-mode nodes with replicas of the old cluster 1:1),
`failover-takeover` (promotion of new replicas to the master via graceful failover),
`forget-external` (discard old nodes).

> **★ Idempotency.** `create`/`add-node`/`remove-node`/`join-external`/
> `failover-takeover`/`forget-external` **idempotent** - re-apply on
> converged input gives `changed=false` (no-op), it is safe to keep them in converge.
> **`reshard` - NO.** This is an imperative **exec-style** day-2 operation (without `unless`):
> applying again will shift **more** `slots` slots from `from` to `to`. The operator is calling
> reshard **explicitly**, exactly as many times as transfers are needed; reshard **not** part
> convergence loop.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `action` | string | required | `create` / `add-node` / `remove-node` / `reshard` (**NOT idempotent**) / `join-external` / `failover-takeover` / `forget-external`. |
| `password` | string (secret) | optional | See "Password". Applies when connecting to **each** node (`create`) / to `new_node`+`seed`+`master` (`add-node`) / to `node`+`seed`+remaining masters (`remove-node`) / to `from`+`to` (`reshard`) / to the new `nodes`+`source_nodes` (`join-external`/`failover-takeover`/`forget-external` - **common** password of the old and new cluster). |
| `username` | string | optional | ACL-username for `AUTH`. |

### cluster — params (`action: create`)

Assembles a cluster from the `nodes` set: `CLUSTER MEET` (gossip) → `CLUSTER ADDSLOTS`
masters → `CLUSTER REPLICATE` replicas. Idempotent - repeat call to already
formed cluster (`cluster_state:ok`, composition coincided, 16384 slots
covered) gives `changed=false`, no-op.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `nodes` | map | required (`create`) | Nodes: map stable-key (SID/name) → `{ addr: "host:port" }` or `{ ip: "10.0.0.1", port: 6379 }`. **The keys are sorted** - they determine the master/replica layout. `addr` - for connection, `ip`+`port` - for `CLUSTER MEET` (gossip operates `ip:port`, not a DNS name). |
| `replicas_per_shard` | int | optional (default `0`) | Replica to the shard. `shards = len(nodes) / (1 + replicas_per_shard)`; `len(nodes)` must be divided by the size of the shard without a remainder. |

**Deterministic layout.** `nodes` keys are sorted; first `shards`
nodes are masters, the rest are round-robin replicas to the masters (`replica j →
master j%shards`). 16384 slots are divided equally between masters; remainder
(`16384 % shards`) is allocated one slot to the first masters. One and the same
same input `nodes` always gives the same topology and the same ranges
slots.

**Gossip convergence** - limited retry (not infinite loop): after `MEET`
the plugin waits until `CLUSTER NODES` shows all nodes, and only then sends
`ADDSLOTS`/`REPLICATE`. If the limit is not met - `failed`.

### cluster — params (`action: add-node`)

Attaches **one** new node to an already formed cluster (`CLUSTER MEET`
via `seed` → `CLUSTER REPLICATE` to master with `role: replica` or empty
master at `role: master`). Idempotent (`CLUSTER NODES`): node is already in the cluster →
`changed=false`, no-op. `role: master` adds **empty** master with no slots -
moving slots is a separate `reshard` (add-node does not move slots).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `new_node` | map | required (`add-node`) | Attached node: `{ addr: "host:port" }` or `{ ip, port }`. `addr` - for connection (`REPLICATE` is executed on it), `ip`+`port` - for `CLUSTER MEET`. |
| `seed` | map | required (`add-node`) | Any existing cluster node is a contact for `MEET` and a source for `CLUSTER NODES` (idempotency): `{ addr: "host:port" }` or `{ ip, port }`. |
| `role` | string | optional (default `replica`) | Newbie role: `replica` (`CLUSTER REPLICATE` to `master` or least loaded) or `master` (empty master with no slots). |
| `master` | map | optional (`role: replica`) | Master, whose replica the newcomer will be: `{ addr: "host:port" }` or `{ ip, port }`. Not specified → the plugin selects the master with the smallest number of replicas (balancing as `redis-cli` without `--cluster-master-id`). |

### cluster — params (`action: remove-node`)

Outputs **one** node from an already formed cluster (mirror `redis-cli
--cluster del-node`/`reshard`, but entirely through go-redis). The plugin reads `CLUSTER
NODES` from `seed` and branches according to the role of the node to be deleted:

- **master with slots** - first **migration of slots** to the remaining masters
(round-robin by their sorted node-id, deterministic): per slot
`CLUSTER SETSLOT <slot> IMPORTING <src-id>` on target → `MIGRATING <dst-id>` on
source → transferring keys in batches (`CLUSTER GETKEYSINSLOT` + `MIGRATE … KEYS …`,
online - data is not lost) → `CLUSTER SETSLOT <slot> NODE <dst-id>` on both
nodes. Then `CLUSTER FORGET <remove-id>` for all the remaining ones.
- **replica or master without slots** - just `CLUSTER FORGET <remove-id>` for all
remaining nodes (slots do not move).

Idempotent (`CLUSTER NODES`): the node is no longer in the cluster → `changed=false`, no-op.
`FORGET` on an already forgotten node on a separate node (gossip-anti-entropy) is interpreted as
no-op, not an error. `MIGRATE` to the password-protected destination carries `AUTH <pass>`
**on the wire** (like go-redis itself) is the only place; in events/logs/errors
the password is not included (see "Password"). Decommission of the host itself (stopping redis,
cleaning `nodes.conf`) is **outside** of this operation.

> **★ Partial-failure (no auto-rollback).** For master **with slots** migration
> (`SETSLOT IMPORTING`/`MIGRATING` → `MIGRATE` → `SETSLOT NODE`) - the same
> non-atomic, no rollback, same as `reshard`. If the operation fails **after**
> `SETSLOT IMPORTING`/`MIGRATING`, but **before** the final `SETSLOT NODE` (break at
> `MIGRATE`, error in the middle of multi-batch slot `> 100` keys), the slot will get stuck in
> suspended IMPORTING(target)/MIGRATING(source), already migrated slots -
> **remain** carried over, `FORGET` is **not** executed yet, apply will return `failed`,
> cluster is in an inconsistent intermediate state. Recovery **manual**:
> check `CLUSTER NODES`, on stuck slots or `CLUSTER SETSLOT <slot>
> STABLE`, or repeat `remove-node` (it will migrate the remainder and complete
> `FORGET`). This is the **conscious semantics** of an imperative operation (like `redis-cli
> --cluster`), **not a bug**.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `node` | map | required (`remove-node`) | Output node: `{ addr: "host:port" }` or `{ ip, port }`. If this is a master with slots, the slots are first migrated to the remaining masters. |
| `seed` | map | required (`remove-node`) | Any existing cluster node is a contact for `CLUSTER NODES` (topology + idempotency) and a source of a list of nodes for `FORGET`: `{ addr: "host:port" }` or `{ ip, port }`. |

### cluster — params (`action: reshard`)

Moves `slots` hash slots from master `from` to master `to` (mirror of `redis-cli
--cluster reshard`, entirely through go-redis). The plugin reads `CLUSTER NODES` from `from`,
takes **the first `slots` source slots in ascending order** and moves each: to
targets `CLUSTER SETSLOT <slot> IMPORTING <from-id>` → on source `MIGRATING
<to-id>` → moves keys in batches (`CLUSTER GETKEYSINSLOT` + `MIGRATE … KEYS …`,
online - data is not lost, **whitespace key names** move as one
argument due to typed `GetKeysInSlot`) → `CLUSTER SETSLOT <slot> NODE
<to-id>` on both nodes.

> **★ reshard is NOT IDEMPOTENT (consciously).** Repeated apply will move **more**
> `slots` slots from `from` to `to` is an imperative exec-style day-2 operation,
> **not** part of converge. No `unless`/probe "already transferred": operator responds
> for how many times he calls her. L0(`cluster_test.go`) proves
> **sequence** of commands and lossless on fake-conn, but does not "prove"
> idempotency - it's not here by design. Real change of slot owner and
> key transfer (incl. whitespace+TTL) on a live cluster is checked by **L3c**
> (`cluster_reshard_l3c_test.go`, build-tag `e2e_live`, `t.Skip` before harness).

Input errors (`from`/`to` - not master in the cluster, `from == to`, `slots < 1`,
`slots` is greater than the number of slots at the source) → `failed`, migration has not started.

> **★ Partial-failure (no auto-rollback).** Slot migration is **not atomic** and **not
> rolls back**. If the operation fails **after** `CLUSTER SETSLOT IMPORTING`
> (target) / `MIGRATING` (source), but **before** the final `SETSLOT NODE` (break at
> `MIGRATE`, error in the middle of multi-batch slot `> 100` keys), this slot
> will be stuck in IMPORTING(`to`)/MIGRATING(`from`) limbo, already
> previously transferred slots - **remain** transferred, apply will return `failed`,
> cluster is in an inconsistent intermediate state. Recovery **manual**:
> check `CLUSTER NODES`, on stuck slots or finish off `CLUSTER SETSLOT
> <slot> STABLE`, or repeat `reshard` (it will finish the remainder). This is **conscious
> semantics** of the imperative operation (like `redis-cli --cluster`), **not a bug**.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `from` | map | required (`reshard`) | Master-**source** of slots: `{ addr: "host:port" }` or `{ ip, port }`. Must be a master in the cluster and own `>= slots` slots. |
| `to` | map | required (`reshard`) | Master-**recipient** of slots: `{ addr: "host:port" }` or `{ ip, port }`. Must be a master in the cluster and different from `from`. |
| `slots` | int | required (`reshard`) | How many slots to transfer (`>= 1`). The first `slots` source slots are taken in ascending order. **Not idempotent** - repeated apply will transfer another `slots`. |

### cluster - live migration between clusters (`join-external` → `failover-takeover` → `forget-external`)

Three steps transfer the load from the **old** cluster-mode cluster to the **new** without
downtime (mirror manual `redis-cli --cluster add-node`/`--cluster failover`/
`--cluster del-node`, entirely via go-redis). Both clusters are on the **same** network under
**single** password/TLS (operator aligns `new == old` before migration). Order
strict: `join-external` (replicas are catching up with the old masters) → `failover-takeover`
(promotion, slots are moving to new ones) → `forget-external` (old nodes are forgotten).
Implementation - [`migrate.go`](../../../../examples/module/soul-mod-community-redis/migrate.go).

**`join-external`** — merge new nodes into the old cluster and make each one a replica
old master **1:1**: connection to `source_nodes` → `CLUSTER NODES` old cluster
→ mapping new-node↔old-master (nodes by key `nodes`, masters by first slot)
→ `CLUSTER MEET` old-seed + waitConverge + `CLUSTER REPLICATE` on **each** new
node. **Fail-fast**: number of old masters `!= shards_dest` → 1:1 not possible
(runtime-assert, `shards_source` is not visible in the render phase). Idempotent: the node is already
replica of the desired master → no-op.

**`failover-takeover`** — promote new nodes (replicas of old masters after
`join-external`) to the master via **graceful** `CLUSTER FAILOVER`. **First
sync-gate**: on **each** new node `INFO replication` `master_link_status == up`
(the replica caught up with the old master) - at least one didn't catch up → **error before the first
failover** (early failover loses its tail). Then on each node graceful `CLUSTER
FAILOVER` (no arguments: master stops recording + sends tail, lossless) →
poll to `role==master` with slots. **Fail-closed**: graceful did not meet the limit →
**error, WITHOUT** escalation to `FORCE`/`TAKEOVER` (aka split-brain). Idempotent:
node is already master → no-op.

**`forget-external`** — throw out old nodes: connect to `source_nodes` → `CLUSTER
NODES` from the old cluster → **all** old node IDs (masters **and** replicas) → `CLUSTER
FORGET <old-id>` on **every** new node. **Without** migration of slots (new ones already have slots
masters after `failover-takeover`). Idempotent: the old id is no longer known to the node
(`Unknown node`) → swallowed as no-op. Decommission of the old hosts themselves (stop
redis, cleaning `nodes.conf`) is **outside** of this operation.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `nodes` | map | required | **New** nodes (map stable-key → `{ addr }` / `{ ip, port }`). `join-external` maps them 1:1 to old masters (keys ↔ masters), `failover-takeover` promotes, `forget-external` performs on every `CLUSTER FORGET`. |
| `source_nodes` | list | required | Seed nodes of the **old** cluster (list `host:port`). They go through in order - the first one to respond, `CLUSTER NODES`, sets the topology. Same password/TLS as new nodes. |
| `shards_dest` | int | required (`join-external`) | Expected number of destination shards (`>= 1`). It must match BOTH the number of new nodes (`nodes`) AND the number of masters of the old cluster - otherwise 1:1 mapping is impossible (fail-fast; assert in Apply, because `shards_source` is visible only in the live topology). |

## replica — params

Links the instance to the master via `REPLICAOF` (go-redis). `masterauth`
is set to `CONFIG SET` **before** `REPLICAOF` (the replica must know the master's password).
Idempotent (`INFO replication`): already a replica of the desired master with a healthy
link → `changed=false`, no-op.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | The address of **this** instance is `host:port` or `unix:/path`. On the redis host local (`127.0.0.1:6379`). |
| `master_addr` | string | required | Master address `host:port`. **HOST-INVARIANT** (one per cluster) - scenario resolves it with run_once (`soulprint.hosts[0]`). `addr == master_addr` → instance is master, `changed=false`, no-op (guard in the plugin so that the scenario calls `replica` on all hosts; with `source_external: true` guard **disabled**). |
| `password` | string (secret) | optional | The password of the master of **his** incarnation. Placed as `masterauth` to `REPLICAOF`. Empty → `masterauth` is not placed. See "Password". With `source_external: true` `masterauth` is taken **not** from here, but from `master_password`. |
| `username` | string | optional | ACL-username for replication of its incarnation (`CONFIG SET masteruser`). When `source_external: true` `masteruser` is taken from `master_username`. |

### replica — params (`source_external`)

Binding to an **external** master (someone else's incarnation / migration), and not to your own host
incarnations are the first step in migrating data from old Redis. At `source_external: true`:
(1) self-guard `addr == master_addr` **disabled** (the external address is obviously not yours);
(2) `masterauth` is taken from `master_password` (not `password`); (3) `masteruser` - from
`master_username`. Further migration proceeds through `offset-synced` (catching up) → `detached`
(promotion). The TLS of the outgoing replication link to the source is enabled by `master_tls`.

> **★ TLS link to the source requires render on disk.** `master_tls: true` includes
> `CONFIG SET tls-replication yes` **to** `REPLICAOF`, but CA/cert/key of Redis source
> reads **from disk along the way**, not inline. Plugin files **not** writes: `master_tls_ca`/
> `master_tls_cert`/`master_tls_key` should put scenario replicas on disk (via
> `core.file.rendered`) and specify paths via `config`-state (`tls-ca-cert-file`/
> `tls-cert-file`/`tls-key-file`) **before** `replica`-step. Otherwise server-cert verification
> source with a handshake will fail. The plugin does not convert PEM values ​​themselves into paths.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `source_external` | bool | optional (default `false`) | `master_addr` points to external master (migration). `true` - enables `master_*` details and removes self-guard. |
| `master_password` | string (secret) | optional | External master source password (`CONFIG SET masterauth`). vault-ref, keeper resolves to Apply. Masked. Empty → `masterauth` is not set. |
| `master_username` | string | optional | ACL-username of the external source for replication (`CONFIG SET masteruser`). |
| `master_tls` | bool | optional (default `false`) | The source receives the replica over TLS. `true` → `CONFIG SET tls-replication yes` to `REPLICAOF`. Requires rendering the source CA/cert to disk (see sidebar). |
| `master_tls_ca` | string (secret, PEM) | optional | PEM CA of an external source (checking its server-cert on the replication link). Masked. Placed on disk by render, path via `config`-state. |
| `master_tls_cert` / `master_tls_key` | string (secret, PEM) | optional | PEM client-cert/key replicas for mTLS on a replication link to the source (only together). Masked. Used as `tls-cert-file`/`tls-key-file` by render, not by plugin. |

## detached — params

Detaches the instance from the master via `REPLICAOF NO ONE` (go-redis), promoting it to
independent master. **The final** step of migration from an external source is after
`offset-synced` confirmed catch-up (`caught_up == true`). Idempotent (`INFO
replication`): instance already `role == master` → `changed=false`, no-op (safe for
I will repeat). Implementation - [`detach.go`](../../../../examples/module/soul-mod-community-redis/detach.go).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | The address of **this** instance is `host:port` or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref, keeper resolves to Apply. Masked; `REPLICAOF` is not passed to arguments (only goes to the connection). |
| `username` | string | optional | ACL-username for the connection (if not default-user). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

**Output**: `changed` (bool) — whether the instance was promoted; `previous_master`
(line `host:port`) - the previous master for auditing (`""`, if the instance was already master
or `master_host`/`master_port` fields were missing).

## sentinel — params

Reconstructs Redis Sentinel **entirely via go-redis** (without `redis-cli`):
`SENTINEL MONITOR`/`REMOVE`+`MONITOR` (monitor) → `SENTINEL SET` (per-master) →
`SENTINEL CONFIG SET` (globals). The source of what you want is `config` (directives in
**file form** `sentinel.conf`); the plugin itself divides them into globals/per-master
(top-level `CONFIG` is not supported in Sentinel mode). The reconcile algorithm has three steps
(`classify_config`/`compute_monitor_action`/`compute_set_updates`). Idempotent
(diff vs `SENTINEL MASTER`/`CONFIG GET`).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | The Sentinel instance address is `host:port` (usually `127.0.0.1:26379`). |
| `master_name` | string | optional (default `mymaster`) | Logical name of the monitored master. |
| `monitor` | map | optional | Desired master address: `{ ip, port, quorum }`. `ip` - **HOST-INVARIANT**. Not specified → the monitor is not touched (only `SET`/`CONFIG SET`). |
| `config` | map | optional | Sentinel directives in **file form** (`"sentinel down-after-milliseconds mymaster": "12000"`, `"sentinel announce-ip": "10.0.0.1"`, `"loglevel": "notice"`). Startup-only (`dir`/`port`/`tls-*`) are ignored - they are changed by restart. |
| `auth_user` | string | optional | User for AUTH Sentinel on master (`SENTINEL SET auth-user`). Set when creating/recreating a monitor. |
| `auth_pass` | string (secret) | optional | Password for AUTH Sentinel on master (`SENTINEL SET auth-pass`). Masked - **not** included in events/logs. |
| `redis_version` | string | optional | Redis version for version-gate global parameters (`loglevel` available in Sentinel since 7.0). Not specified → version-gated parameters are discarded. |
| `password` | string (secret) | optional | Password for connecting **to** Sentinel itself (its `requirepass`), if specified. See "Password". |
| `username` | string | optional | ACL-username for connecting to Sentinel. |

## Password (IS-invariant ADR-010)

Plugin capacity - **only `network_outbound`**, `vault_access` **no**:
the password comes already resolved from Keeper. In operator-input
(scenario/destiny) the password is set by vault-ref via CEL `${ vault(...) }`;
keeper-side render phase resolves it **to** `Apply` and passes it to the plugin
plaintext value (ADR-012 - Soul/Vault client plugin does not work). In the manifesto
`password` is marked `secret: true` + `pattern: "^vault:.*"` - this forces
vault-ref at the input and masking in logs/trace/UI.

Code invariant (checked by L0): `params["password"]` (and `auth_pass` in `sentinel`,
`master_password` to `replica source_external`, `source_password` to `offset-synced`)
**never** get into `ApplyEvent.Message`/`.Output`, error text and
stderr. Connection errors are sanitized (`redactError` cuts out the substring
password - during the second connection, `offset-synced` edits **both** passwords); conclusion
of the commands themselves (`result`) is the server's response, not the operator's secret. In `sentinel`
Output carries only the **names** of the applied actions (`sentinel_monitor`/`sentinel_set`/…),
not their secret values; `replica` sets `masterauth` as argument to `CONFIG SET`
(Redis is needed for synchronization), but it does not write it to events.

## TLS connection (`redis_tls_*` parameters)

All states (`command`/`pinged`/`role`/`replica-synced`/`offset-synced`/`config`/`acl`/
`cluster`/`replica`/`detached`/`sentinel`) accept **general** TLS connection parameters.
TLS is disabled by default (plaintext, back-compat); at `tls: true` the plugin connects
to Redis over TLS. Two states have a **second** set of TLS parameters for the external link:
`offset-synced` — `source_tls`/`source_tls_ca` (connection to an external source),
`replica source_external` — `master_tls`/`master_tls_ca`/`master_tls_cert`/`master_tls_key`
(outgoing replication link of the replica to the source; see sidebar in ["replica `source_external`"](#replica--params-source_external)).

| Parameter | Type | Default | Destination |
|---|---|---|---|
| `tls` | bool | `false` | Connect via TLS. **Required in only-TLS** (Redis `port 0`, plain closed): without it the plugin will not reach you. |
| `tls_ca` | string (secret, PEM) | — | CA certificate for server verification (RootCAs). With private PKI it is almost mandatory. |
| `tls_cert` | string (secret, PEM) | — | Client certificate for mTLS (optional, **only together** with `tls_key`). |
| `tls_key` | string (secret, PEM) | — | Client key for mTLS (optional, **only together** with `tls_cert`). |
| `tls_skip_verify` | bool | `false` | **EXPLICIT opt-out** checking server certificate. By default, the check is **enabled** (default secure). |

Security model (security-invariant: insecure = explicit opt-out, default secure):
with `tls: true` the default plugin **checks** the server certificate (`RootCAs`
from `tls_ca`). You can disable the check **only** with an explicit `tls_skip_verify: true`.
`tls_cert`+`tls_key` are specified strictly together (one without the other → validation error
configuration, without leaking PEM to text).

PEM comes **entirely** into params: scenario resolves it from Vault via
`${ vault(...) }` in the render phase and puts PEM in `apply.input` (like `requirepass`) -
the plugin does not support its Vault access (capability remains `network_outbound`). B
manifest `tls_ca`/`tls_cert`/`tls_key` are marked `secret: true` + `pattern:
"^vault:.*"` (secret source declaration); masking - by **key name**:
`shared/audit` masks `tls_key`/`tls_cert`/`tls_ca` in logs/OTel/RunResult/UI.
Code-invariant (L0): PEM client key does not fall into `ApplyEvent`/connection errors
(TLS-handshake error is sanitized by `redactError` by `password` **and** PEM key).

**Mutual cluster-bus supported.** With mutual authentication of nodes (Redis
`tls-cluster yes`) nodes require a client certificate from the one who accesses them
connects. Scenario of service `redis` in step `cluster` `action: create`
forwards `tls_cert`/`tls_key` to the plugin (resolved from the same
`vault(essence.tls_cert_ref/tls_key_ref)`, same as server PEM redis.conf) -
plugin builds mTLS pair (`tls.go`: client-cert is added when **both** are specified
`tls_cert`+`tls_key`). Without mutual-bus these parameters do not interfere with handshake
(only used if the server requested them). `cert`/`key` host-invariant
(one per cluster) → correctly go through `apply.input`.

**Anti-downgrade (IS).** Plugin connections in scenario `redis` are gated to
`essence.tls_enable`, **not** `tls_only`: when `tls_enable: true` the plugin connects
via TLS even when the plain port is still open (`tls_only: false`). Otherwise the AUTH password is gone
over the plaintext network (plaintext-downgrade). Connection port - `tls_port` when
`tls_enable`, otherwise plain `6379`.

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` - TCP/unix/**TLS** connection to Redis
(for `cluster` - connection to each node from `nodes`, for cluster live migration - more
and to `source_nodes` of the old cluster; for `offset-synced` - **second** connection to
external `source_addr`; for `replica source_external` — outgoing replication link
to external master). **Without** `vault_access` (password and PEM resolve Keeper), **without**
`exec_subprocess` / `fs_write_root` (plugin does not start subprocesses - `cluster`
goes entirely through go-redis, not through `redis-cli` - and does not write to FS; CA/cert files
external source for TLS migration puts scenario via `core.file.rendered`).
- `side_effects: [{ service: redis-server }]` - all state are working on live
by the redis service.

## Example call from scenario

```yaml
# Apply the final redis_config to live Redis after render redis.conf destiny.
- name: Apply redis runtime config
  module: community.redis.config
  params:
    addr: "127.0.0.1:6379"
    # The password is resolved by keeper-side via vault() in the render phase (ADR-012):
    # it's the value that goes into the plugin, not the link.
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    config: "${ state.redis_config }"

# Raw command (probe): changed=false by default.
- name: Ping redis
  module: community.redis.command
  register: pong
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    args: ["PING"]
```

Migration from external Redis (three steps, health-gate by `caught_up`):

```yaml
# 1. Link the local instance with a replica to the EXTERNAL master.
- name: Replicate from external source
  module: community.redis.replica
  params:
    addr: "127.0.0.1:6379"
    master_addr: "${ input.source_addr }"
    source_external: true
    master_password: "${ vault('secret/redis/legacy#password') }"

# 2. Wait for the complete data catch-up (not just a live link).
- name: Wait until caught up with source
  module: community.redis.offset-synced
  register: sync
  until: register.self.caught_up == true
  retry: { attempts: 60, delay: 5 }
  params:
    addr: "127.0.0.1:6379"
    source_addr: "${ input.source_addr }"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    source_password: "${ vault('secret/redis/legacy#password') }"

# 3. Untie and promote to a separate master (migration final).
- name: Detach and promote to master
  module: community.redis.detached
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
```

## Tests

- **L0 command/config/acl**
  ([`impl_test.go`](../../../../examples/module/soul-mod-community-redis/impl_test.go)):
fake `redisConn` + fake `ApplyEvent`-stream. Covers `Validate` (empty
addr/args/config, acl requires addr, unimplemented state), Apply happy-path
command/config, unix-socket-parsing, `changed`-semantics, numeric stringification
values, `CONFIG REWRITE`; **startup-only-denilist** (`config` skips
`port`/`dir`/`aclfile`/`cluster-enabled`/`loadmodule` - neither `CONFIG GET` nor `SET`
are not called by it, `skipped`/`skippedCount` in Output are correct; all-startup-only →
`changed=false`, none `SET`); **acl** (`ACL LOAD` is sent between `ACL LIST`
before/after, `changed=true` on diff / `false` on match, error `LOAD`/`LIST` →
`failed`); and **IS-invariant** - the password does not leak into events or arguments
commands, nor in a sanitized connection error.
- **L0 probe (pinged/role/replica-synced)**
  ([`probe_test.go`](../../../../examples/module/soul-mod-community-redis/probe_test.go)):
fake `redisConn`. Covers `Validate` (empty `addr`); `pinged` happy-path
(`PING` → `Output.result == 'PONG'`, `changed=false`), error `PING` → `failed`;
  `role` happy-path (`INFO replication` → `Output.role` = `master`/`slave`,
`changed=false`), `INFO replication` without field `role` → `failed`; `replica-synced`
(`master_link_status: up` → `synced=true`; field missing → `synced=false` with
reason); **IS-invariant** (password does not flow into events/sanitized error
connection).
- **L0 offset-synced**
  ([`offset_synced_test.go`](../../../../examples/module/soul-mod-community-redis/offset_synced_test.go)):
fake `redisConn` (own + external source). Covers `Validate` (requires `addr` +
`source_addr`, rejects negative `lag_threshold`); `caught_up=true` when
catching up; `lag > threshold` / `lag <= threshold`; `master_sync_in_progress` → not
caught_up; `link down` → not caught_up; lack of offset → not caught_up; opt.
`DBSIZE`-checksum and `skip_checksum`; that the **second** connection uses `source_*`-
details and `source_tls` (regardless of its TLS); **IS-invariant** (neither yours nor
source password does not leak - incl. when the second connection fails).
- **L0 cluster**
  ([`cluster_test.go`](../../../../examples/module/soul-mod-community-redis/cluster_test.go)):
fake-fleet of nodes by addr. Covers `Validate` (empty `nodes`, non-`create`
action, indivisible composition, negative `replicas_per_shard`); happy create
(`MEET`/`ADDSLOTS`/`REPLICATE` with correct arguments, full coverage
16384 slots, roles are deterministic); already-formed → `changed=false`, no-op;
determinism of layout on several runs; sorting roles layout
keys; dividing slots with remainder; **add-node** (replica auto/explicit master,
empty master, idempotency); **remove-node** (replica → `FORGET` only;
master with slots → migration of slots `SETSLOT`/`MIGRATE`/`SETSLOT NODE` +
`FORGET`; empty master → only `FORGET`; idempotency "node no longer exists" →
no-op); **reshard** (`Validate` - empty `from`/`to`/`slots`, `from == to` on.
mixed `{addr}`/`{ip,port}`-form, `slots < 1`; happy-transfer - first N
source slots in ascending order via `SETSLOT IMPORTING`/`MIGRATING`/`MIGRATE`/
`SETSLOT NODE` on both nodes; **whitespace-lossless**; `from` not master →
`failed`; `slots` more than available → `failed`); **IS-invariant** (password not
flows into events/commands/connection error; the only wire-AUTH is in `MIGRATE`,
is checked by a separate assert).
- **L3c reshard skeleton**
  ([`cluster_reshard_l3c_test.go`](../../../../examples/module/soul-mod-community-redis/cluster_reshard_l3c_test.go)):
e2e-live vs real cluster (build-tag `e2e_live` + `t.Skip` to
harness-entity "live redis cluster"). TODO-invariant: writing keys to slots
source (incl. whitespace+TTL), one imperative reshard, real check
slot owner changes + lossless keys + TTL + convergence `DBSIZE`.
Compiled in a gate, it really doesn't run without a live cluster.
- **L0 replica**
  ([`replica_test.go`](../../../../examples/module/soul-mod-community-redis/replica_test.go)):
fake `redisConn` with scripted `INFO replication`. Covers `Validate`
(no `master_addr`); `REPLICAOF` + `masterauth` BEFORE it; idempotency (already
replica of the desired master → no-op); `addr == master_addr` → master-guard no-op
(no commands); empty password → `masterauth` is not set; `source_external`
(self-guard removed, `masterauth`/`masteruser` from `master_*`, `tls-replication yes`
at `master_tls`); **IS-invariant** (neither `password` nor `master_password` flows
in events/sanitized connection error).
- **L0 detached**
  ([`detach_test.go`](../../../../examples/module/soul-mod-community-redis/detach_test.go)):
fake `redisConn` with scripted `INFO replication`. Covers `Validate` (empty
`addr`); slave → `REPLICAOF NO ONE` + `changed=true` + `previous_master` in Output;
already master → no-op (`changed=false`, no commands); error `INFO` → `failed`;
**IS-invariant** (the password does not flow into the sanitized connection error).
- **L0 cluster live migration (join-external/failover-takeover/forget-external)**
  ([`migrate_test.go`](../../../../examples/module/soul-mod-community-redis/migrate_test.go)
  + [`migrate_failover_test.go`](../../../../examples/module/soul-mod-community-redis/migrate_failover_test.go)):
fake-fleet of nodes (new + old cluster by `source_nodes`). `join-external`:
`Validate` (empty `nodes`/`source_nodes`/invalid `shards_dest`); happy 1:1-
mapping nodes↔masters (by **first slot**, not node-id); fail-fast with mismatch
number of masters / number of nodes and `shards_dest`; idempotency (node is already a replica →
no-op), partial-idempotency; failover seed node to the next one; no-leak if fail
source-connection. `failover-takeover`: **sync-gate** blocks until **first**
failover, if at least one node has not caught up; **fail-closed** without escalation to
`FORCE`/`TAKEOVER`; idempotency (node ​​is already master → no-op), partial. `forget-
external`: `FORGET` all old ids on **each** new node; doesn't foreget himself
(`Cant forget self` swallowed); **without** slot migration; `Unknown node` → no-op;
seed-failover and "all seeds are gone" → `failed`. In all - **IS-invariant** (password
does not leak).
- **L0 sentinel**
  ([`sentinel_test.go`](../../../../examples/module/soul-mod-community-redis/sentinel_test.go)):
fake `redisConn` with scripted `SENTINEL MASTER`/`CONFIG GET`. Covers
pure transfer functions (`classifyConfig`/`supportedGlobals` version-gate +
secret-filter/`computeMonitorAction`/`computeSetUpdates`); `Validate`; `MONITOR`
  + auth-set for new monitor; idempotency (address matched → no-op); `readd`
(`REMOVE`+`MONITOR`) when changing the address; per-master `SET` reconcile (only
differences); globals `CONFIG SET` reconcile; **IS-invariant** (`auth_pass` does not flow
in events/connection error).
- **L1** (integration, testcontainers redis/sentinel) - next batch.

`GOWORK=off go test ./...`.

## Assembly

```sh
cd examples/module/soul-mod-community-redis
GOWORK=off go build ./...   # binary soul-mod-community-redis (gitignored)
GOWORK=off go test ./...    # L0
```

Module - separate go.mod from `replace` to core (`../../../proto/plugin`,
`../../../sdk`); going standalone, not included in `go.work` (convention
`examples/module/`).

## See also

- [README.md](../../README.md) - module directory (directory status).
- [examples/service/redis/](../../../../examples/service/redis/) - redis service:
  scenario `create` (standalone/cluster/sentinel), `add_node`, `remove_node`,
`reshard` (day-2, **NOT idempotent**) and day-2 hot-reload `update_config`
  (→ state `config`), `add_user` (→ state `acl`), `rotate_tls` (→ state `command`,
force re-read SSL_CTX), `migrate_cluster` (→ `cluster` live migration + `replica`
  `source_external` + `offset-synced`), `detach_source` (→ `detached` + `offset-synced`)
call the states of this plugin.
- [examples/destiny/redis/](../../../../examples/destiny/redis/) —
mode-agnostic per-host brick (install + render `redis.conf` + systemd).
- [ADR-012](../../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, password
reaches the value.
- [ADR-031 Scry](../../../adr/0031-scry-drift.md) — default-deny on dry_run without
  `PlanReadSafe`.
- [templating.md](../../../templating.md) - secret masking (§7.4).
