# redis

> ★ **Re-laid-out by NIM-766.** Every address on this page is
> `redis.<object>.<action>` ([address rule](../../naming-rules.md#the-discipline-binding-the-three-levels)):
> level 1 is the registration alias, level 2 the **object** this plugin manages, level 3 the
> **action**. The grouping level `community.*` is gone — it named where the plugin came from,
> not what it manages. The old form `community.redis.<state>` resolves nowhere; a cluster that
> still registers the artifact as `community` must change the alias in
> `keeper.yml::plugins.*[].name`, which is a config edit and nothing more.
>
> The `redis.user.*` object — ACL users through `ACL SETUSER`/`ACL DELUSER` — landed
> with **NIM-767**. It is not a replacement for `acl.reloaded`: there the subject is
> the aclfile a destiny rendered, here it is one user. Wiring it into a service that
> still renders that file is NIM-768.

MAIN interface to **live Redis** in redis consolidation (role-based concept): the service scenario orchestrates the order/targeting/rolling, and the plugin
performs **one** operation on one Redis instance. Custom plugin
`kind: soul_module` serving **seven objects** — `acl`, `cluster`, `command`, `instance`,
`replica`, `sentinel`, `user` — from one binary, `redis`; the artifact carries no
name of its own (NIM-377), so level 1 is whatever alias the operator registers it under.
Implementation -
[`examples/module/redis/`](../../../examples/module/redis/)
(`object.go` - the object tables and the dispatch, `bundle.go` - the seven `module.Def`s the
schema document is generated from, `obj_*.go` - one file per object's declaration,
`impl.go` - the shared driver + command.run/instance.configured/acl.reloaded, `probe.go` -
the read probes instance.pinged/instance.role-probed/replica.synced/replica.offset-synced,
`cluster.go` + `migrate.go` — the cluster object
(created/node-added/node-removed/resharded + external-joined/failed-over/external-forgotten),
`replica.go` - replica.present (REPLICAOF, external source `source_external`),
`detach.go` — replica.detached (REPLICAOF NO ONE, promotion),
`sentinel.go` — sentinel.monitored (MONITOR/SET reconcile), `user.go` —
user.present/user.absent (ACL SETUSER/DELUSER on one user), `helpers.go` —
structpb/secret-helpers).

Backend — [`github.com/redis/go-redis/v9`](https://github.com/redis/go-redis):
the plugin itself connects to Redis via TCP (`host:port`) or a unix socket
(`unix:/path`). Correspondence to core modules (`core.pkg`/`core.file`/`core.service`/
`core.sysctl`) - for everything that is NOT redis-specific (installation, render
`redis.conf`, systemd); Redis runtime itself is this plugin.

## Without dry-run preview (consciously)

The plugin **doesn't** implement `PlanReadSafe`
([ADR-031](../../adr/0031-scry-drift.md)) and `ErrandReadSafe`
([ADR-033](../../adr/0033-errand.md)): it remains on
`module.BaseModule`. This is a conscious choice (user decision 2026-06-22):
on `dry_run` host (Soul) applies **default-deny** - the task gets honest
"preview not supported" rather than the false "no drift". Better an obvious refusal than
quiet clean from no-op Plan.

## Objects and actions

Seven objects (`schema.json::modules[]`), **nineteen** actions between them. The object is
address level 2 — what is being managed — and the action is level 3: the state it is left
in, or, on the one non-stateful object, the single verb naming the operation.

| Object | Actions | What it manages |
|---|---|---|
| `command` | `run` | An arbitrary Redis command, run as given. Non-stateful, so level 3 is the verb form (`core.exec.run`'s shape). It takes exactly one verb — a second operation would be a second object. |
| `instance` | `pinged`, `role-probed`, `configured` | A live Redis server: is it answering, what role does it hold, and its running configuration. |
| `acl` | `reloaded` | The access-control list of a live instance, reconciled from its aclfile. The subject is the FILE: a destiny renders it, this makes the instance re-read it. |
| `user` | `present`, `absent` | ONE ACL user, reconciled directly through `ACL SETUSER`/`ACL DELUSER`. The subject is the USER, so nothing here renders or re-reads a file and the rest of the ACL is untouched. |
| `replica` | `present`, `detached`, `synced`, `offset-synced` | The replication link of one instance: bound to a master, torn down, or caught up (locally, or against an external source). |
| `cluster` | `created`, `node-added`, `node-removed`, `resharded`, `external-joined`, `failed-over`, `external-forgotten` | A Redis Cluster. Every action here manages SEVERAL nodes, so none takes a single `addr`. |
| `sentinel` | `monitored` | A live Redis Sentinel: the masters it monitors and the options it holds. The one object WITHOUT a keyspace — Sentinel refuses `SELECT`, so `db > 0` breaks the connection outright (NIM-229). |

> **★ The seven cluster actions were one state carrying `params.action` until NIM-766.**
> Splitting them onto level 3 is what lets each declare only the params it reads: before,
> `created` and `resharded` shared one declared input of fifteen, so param strictness had no
> contract to hold either of them to. The `action:` param is **gone** — the operation is the
> address.

| Action | Destination | `changed` |
|---|---|---|
| `command.run` | Raw command for Redis (imperative verb-state, precedent `core.cmd.shell`/`core.exec.run`/`core.http.probe`). | `false` default; `changed: true` in params - for really mutating commands. |
| `instance.pinged` | Health-probe via go-redis `PING` (expecting `PONG`). Read-only. Replaces idiom `command args:[PING]` - health-gate in scripts (`retry`/`until`/`failed_when` by `register.self.result`). | `false` **constructive** (probe, not change). |
| `instance.role-probed` | Role-probe via go-redis `INFO replication` - actual **volatile** role of the instance. Read-only. Used for `where` rolling-restart targeting (ADR-008: the role is volatile, measured by a live probe before targeting). | `false` **constructive** (probe). |
| `replica.synced` | Probe the replica gum via go-redis `INFO replication` (`master_link_status == "up"`). Read-only. Only for **replica** (master does not have this field → `synced=false`). Health-gate rolling-restart — wait until the replica is completely reset before restarting the next one. | `false` **constructive** (probe). |
| `replica.offset-synced` | Safety-gate migration from an **external** source: checks `slave_repl_offset` of its instance with `master_repl_offset` of the external master (second connection to `source_addr`). Read-only. "Link is alive ≠ data caught up"; `caught_up=true` only with `link up` + no full-sync running + `lag <= lag_threshold`. | `false` **constructive** (probe). |
| `instance.configured` | Apply map directives `redis.conf` through `CONFIG SET` (+ optional `CONFIG REWRITE`). Startup-only directives (`port`/`dir`/`aclfile`/… - denilista) **are skipped** (CONFIG SET rejects them). | `true` with ≥1 directive applied. |
| `acl.reloaded` | Hot-reload ACL of live Redis via `ACL LOAD` (re-read `aclfile` in its entirety - `users.acl` renders destiny BEFORE this step). Idempotent **by design**; the output `ACL LIST` is not** included in Output (it may carry a password-hash). | `true`/`false` by diff `ACL LIST` before/after `LOAD` (matched → `false`, no-op). |
| `cluster.created` | Build a hash-slot cluster (16384 slots) from scratch via `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE`. | Idempotent: `true` on change, `false` (no-op) on converged input. |
| `cluster.node-added` | Attach one node to a formed cluster (day-2). | Idempotent: `true` on change, `false` (no-op) if the node is already in. |
| `cluster.node-removed` | Evict one node (day-2), migrating a master's slots to the remaining masters first (`SETSLOT`/`MIGRATE`), then `CLUSTER FORGET`. | Idempotent: `true` on change, `false` (no-op) if the node is already gone. |
| `cluster.resharded` | Transfer N slots master→master (day-2). | **NOT idempotent** (see below): `true` on successful migration, `failed` on input error; there is no no-op branch. |
| `cluster.external-joined` | Live migration step 1: add the new nodes as replicas of the old cluster's masters 1:1. | Idempotent: `true` on change, `false` (no-op) on converged input. |
| `cluster.failed-over` | Live migration step 2: promote those replicas to masters via graceful `CLUSTER FAILOVER`. | Idempotent: `true` on change, `false` (no-op) if already master. |
| `cluster.external-forgotten` | Live migration step 3: `CLUSTER FORGET` the old nodes. | Idempotent: `true` on change, `false` (no-op) on an unknown node. |
| `replica.present` | Link the instance to the master via `REPLICAOF` (+ `CONFIG SET masterauth`). Opt. `source_external: true` - binding to the **external** master (migration) with separate details `master_*`. | `true` when configuring; `false` (no-op), if there is already a replica of the desired master or `addr == master_addr` (the master itself, guard is disabled with `source_external`). |
| `replica.detached` | Detach the instance from the master via `REPLICAOF NO ONE`, promoting it to an independent master. The final step of migration from an external source (after `offset-synced` confirmed catch-up). Idempotent: already master → no-op. | `true` during promotion; `false` (no-op), if the instance is already master. |
| `sentinel.monitored` | Reconcile Redis Sentinel (`SENTINEL MONITOR`/`REMOVE`/`SET`/`CONFIG SET`). Reconcile algorithm: classify config → monitor/set diff. | `true` when changing monitor/parameters; `false` (no-op), if everything matches. |
| `user.present` | Reconcile ONE ACL user via `ACL SETUSER` — perms and state as declared, the credential as `#<sha256hex>`. Declarative: the vector opens with `reset`, so a permission dropped from the declaration is dropped on the instance. `ACL SAVE` after a change (`persist`, default `true`). | `true`/`false` by diff of this user's `ACL LIST` line before/after `SETUSER` (matched → `false`, no-op). |
| `user.absent` | Remove ONE ACL user via `ACL DELUSER`. The rest of the ACL is untouched. `ACL SAVE` after a removal that happened. | `true` on removal; `false` (no-op) on a user the instance does not have — and no *mutating* command is sent, nor is the aclfile touched. |

## command.run — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path` (unix socket). |
| `args` | list | required | Command with array of arguments **without shell**: `["CONFIG","SET","maxmemory","256mb"]`. The first element is verb. |
| `password` | string (secret) | optional | Redis password. vault-ref in operator-input, keeper resolves to Apply (see "Password"). Masked. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | The database number (`SELECT`) before the command. |
| `changed` | bool | optional (default `false`) | Mark the result `changed=true` (default probe semantics). |

## instance.pinged — params

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

## instance.role-probed — params

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

## replica.synced — params

Probe replica resin via go-redis `INFO replication`: checks
`master_link_status == "up"` (the replica **has caught up** with the master after the restart).
**Read-only**, `changed=false` constructive. Stricter `instance.pinged` (`PONG` means only
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

## replica.offset-synced — params

Safety-gate migrating a replica from an **external** source via go-redis. Stricter
`replica.synced`: "link is alive ≠ data caught up." Checks `slave_repl_offset` **his**
instance (`addr`) with `master_repl_offset` **external** master (**second** connection to
`source_addr` with `source_*` details - it is the authoritative "head" for calculating lag).
**Read-only**, `changed=false` constructive. `Output.caught_up` (bool) - condition
health-gate (`until: register.self.caught_up == true`) of the final `replica.detached` step
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
| `username` | string | optional | ACL-username for `AUTH` on **your** instance. The source connection has no username of its own - `source_password` authenticates as the source's default user. |
| `db` | int | optional (default `0`) | Database number (`SELECT`) on **your** instance. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters for **your** instance (see "TLS connection"). |
| `source_tls` / `source_tls_ca` / `source_tls_cert` / `source_tls_key` / `source_tls_skip_verify` | — | optional | The same set for the **second** connection to the external source - its own CA and verification (the two sides may sit behind different PKI). In practice the migration pilot passes only `source_tls` + `source_tls_ca`. |

**Output**: `caught_up` (bool) — final catch-up condition; `lag_bytes` (int64) —
`master_repl_offset − slave_repl_offset` (negative clamped to `0` -
read-after-write-window, non-negative lag); `master_sync_in_progress` (bool) - running
whether full-sync; with `!skip_checksum` additionally `dbsize_source` / `dbsize_replica`
(rough sanity check of sizes - different DBSIZE on the fly is normal due to TTL/eviction, on
`caught_up` has no effect).

## instance.configured — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address (same as `command.run`). |
| `config` | map | required | Directives `redis.conf`: `{ maxmemory: "256mb", maxmemory-policy: allkeys-lru }`. Each (except startup-only - see below) → `CONFIG SET <key> <value>`. They are applied in a deterministic order (by key). Numeric values ​​are stringified (`20000`, not `20000.000000`). |
| `password` | string (secret) | optional | See "Password". |
| `username` | string | optional | ACL-username for `AUTH`. |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `CONFIG SET`. |
| `rewrite` | bool | optional (default `false`) | After `CONFIG SET` execute `CONFIG REWRITE` (persist in `redis.conf`). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

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
> [`redis/tasks/server.yml`](../../../examples/destiny/redis/tasks/server.yml)).
> Deniliste (`startupOnlyDirectives` in [`impl.go`](../../../examples/module/redis/impl.go)):
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

## acl.reloaded — params

Hot-reload ACL of live Redis: `ACL LOAD` causes the instance to re-read `aclfile`
**entirely**. `users.acl` renders destiny `redis` **before** this step (via
`core.file.rendered`, the plaintext password is not written - `.tmpl` hashes) - `acl.reloaded` only
causes Redis to re-read the finished file. **Idempotent by design**: `ACL LOAD`
brings the live instance to the declared file, regardless of the current state.

`changed`-semantics: `ACL LOAD` itself does not report "changed", so the plugin does
cheap honest diff - `ACL LIST` **before and after** `LOAD` (typed path, string
per user; the order is significant). Matched → `changed=false` (live instance already
matched the file, no-op - symmetry with `instance.configured`/`cluster`/`sentinel.monitored`); different →
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

## user — the object

`acl.reloaded` and `user.*` are not two ways to do one thing. In `acl` the subject is
the **file**: a destiny renders the whole `users.acl` and the plugin makes Redis re-read
it, so every edit runs through both halves and forgetting to merge the service accounts
back into that render wipes replication (the ★★ note in
[`scenario/add_user/main.yml`](../../../examples/service/redis/scenario/add_user/main.yml)
exists for exactly that reason). In `user` the subject is the **user**: `ACL SETUSER`
reaches it directly and the rest of the ACL is nobody's business.

**★ The credential never reaches the wire in the clear.** `ACL SETUSER` takes either
`>plaintext` or `#<sha256hex>`, and this object only ever sends the hash — the same
digest `users.acl.tmpl` has always written (sprig `sha256sum`), so a user written here
and the same user written by the render are indistinguishable to Redis. `user_password`
is `secret: true` with `pattern: "^vault:.*"` like every other secret here; the
plaintext's absence from the argument vector **and** the hash's presence in it are both
guard tests (`user_test.go`) — asserting only the absence would pass just as well on a
build that dropped the credential, which is a silent lockout.

**★ `perms` carries permissions, not credentials.** Redis takes both in one rule vector,
and `perms` is a plain declared string — not `secret: true` — so its value is unmasked in
the rendered task, in keeper's logs, traces and UI, and in git. A token carrying a
credential **value** (`>pass`, `<pass`, `#hash`, `!hash`) is therefore **refused**, and
the refusal names the token's prefix and position rather than quoting it, because the
error text lands on the same surfaces.

A **leading `(` is stripped before that test**. Redis merges a selector across
arguments, so `(>secret +get)` splits on whitespace into `(>secret` and `+get)` and the
credential sits at byte 1 — while Redis reads the same rule as one modifier and quotes
the *whole* thing back in its syntax error, which becomes `ApplyEvent.Message`. Missing
it did not merely let a credential through: it put the plaintext on precisely the
surface the refusal protects, somewhere `redactError` cannot reach (the value is in
`perms`, not in a secret param). The mirrored `(+get >secret)` was caught all along, so
one rule got two verdicts depending on how it was written.

**Where the guard stops, stated exactly.** It covers the positions Redis actually
parses a credential in — the start of a token, and the start of a token opening a
selector. It does **not** cover a credential-looking substring buried anywhere else
(`+get(>hunter2`): Redis rejects the modifier and quotes the whole thing back, so the
text lands in `ApplyEvent.Message`. That class is unbounded and cannot be closed by a
token test. It is also not a *new* leak: `perms` is not a secret param, so anything
written there is already in the clear in the rendered task, the logs and git. The
guard's job is to stop a credential from being **set** through a param that cannot
mask it — and no such token ever reaches Redis in a form it would honour.

Three keywords are refused as well, and **in any case** — Redis's ACL parser matches
with `strcasecmp`, so an exact comparison is a one-keystroke bypass, and the first
version of this guard had one: `RESET` walked through it and discarded the declared
perms, state and password on a live instance while the step reported success.

Matching `strcasecmp` means its **other** half too. It stops at the first NUL byte and
Go's `EqualFold` does not, so `reset\0x` reads as `reset` to Redis and as an unknown
token to a comparison that folds case alone — which restored every one of these harms.
The YAML parser this tree uses decodes an embedded NUL into a Go string, so it is
reachable from a scenario. A token carrying a NUL is therefore refused outright rather
than truncated: truncating would leave this object silently reinterpreting what the
author wrote, and only refusing keeps its reading of a token and Redis's the same one.

| Refused in `perms` | Why |
|---|---|
| `reset` | The vector already opens with one; a second discards the state and credentials this step just set. |
| `on` / `off` | That is `state`'s job. A token here lands *after* it and overrides it, and `Output.state` would then report the declaration rather than the instance. |
| `nopass` / `resetpass` **when `user_password` is also declared** | Redis takes the last directive and the perms token is last, so the two contradict silently — the account ends up authenticating with any string, or with none. |

`nopass`/`resetpass` **alone** stay legal: they carry nothing secret and are how you
declare a user that holds no password. They override the carry-over below, which is the
author saying so explicitly instead of an omitted `user_password` doing it by accident.

**★ Every one of these rules is enforced in `Apply`, not only in `Validate`.** `Validate`
is a separate RPC and nothing in this tree calls it before applying — `soul`'s
applyrunner calls `Apply`, and keeper's static check is presence-only and returns early
on a `${…}`-rendered cell. So `perms: "${ vars.acl }"` with the var unset lints clean and
arrives empty; since the vector opens with `reset`, an `Apply` that did not re-check
would strip every permission the live user holds and then `ACL SAVE` it — an account that
still authenticates and answers `NOPERM` to everything. Guard tests drive the real
`Apply` for each rule.

**★ Declarative, hence `reset`.** `ACL SETUSER` MERGES into a user's existing rules, so a
permission dropped from the declaration would stay live and `present` would only ever
converge upward. The rule vector therefore opens with `reset`. That clears the user's
passwords too, which is why an **omitted** `user_password` re-applies the credentials
carried over from the live `ACL LIST` line rather than revoking the one clients already
hold — the `add_user` scenario's "re-running keeps their password", kept mechanically.

**★ NIM-624 has no analogue here — precisely, and no wider.** What that ticket recorded
was a half-written `users.acl` left on disk by a failed `ACL LOAD`, after which Redis
would not start. This path writes no file of its own: `ACL SETUSER` is atomic (Redis
validates the whole rule vector and applies none of it on error), and `ACL SAVE` runs
**only** after a command that both succeeded and changed something. A failed apply
leaves behind no file Redis will refuse to load, and that is a guard test rather than a
claim.

It does **not** say a failed apply changed nothing. `SETUSER` and `SAVE` are two
commands, so a `SAVE` that fails leaves the instance mutated behind a step reporting
failure — and `ApplyEvent` cannot report both: soul's applyrunner tests `GetFailed()`
before `GetChanged()` ([`applyrunner.go`](../../../soul/internal/runtime/applyrunner.go)),
so `changed` on a failed event is unreachable and every handler keyed on it stays
silent. Worse, a re-run then reports a clean **no-op**, because the instance really does
match now — the drift is in the file, where nothing is looking. So the common cause is
refused **before** anything is mutated: with `persist` on, the apply asks
`CONFIG GET aclfile` first and fails without touching the ACL if there is none. What
that pre-flight cannot cover (a full disk, a read-only mount) is named in the `ACL SAVE`
failure text, which says in so many words that the change is live and unrecorded.

The pre-flight is **evidence, never a requirement**. `CONFIG` is `@admin`/`@dangerous`
and `ACL SETUSER`/`ACL SAVE` are not, so `+acl +ping +select` without `+config|get` is
a connection that can do the entire job — and the first version of this refused all of
it with `NOPERM`, work that very connection was able to perform. Only a `CONFIG GET`
that *succeeds* and answers empty is evidence; an error means "cannot tell" and the
apply proceeds as it did before the pre-flight existed. On `user.absent` the check also
runs **after** the probe, because with no user to remove there is nothing to persist
and checking first turned an idempotent cleanup into a hard red.

**★ `persist` and the seam with the render.** `ACL SETUSER` changes memory only; the
`aclfile` on disk still holds what the destiny rendered, so without `ACL SAVE` the user
is silently dropped by the next restart or the next `acl.reloaded`. Hence `persist`,
default `true`. It deliberately does **not** run on a no-op: `ACL SAVE` rewrites the file
in Redis's own rendering of the rules, which a destiny that also renders it reads as a
change and answers with a restart — on every run. A change does leave the file in
Redis's form rather than the template's, and reconciling the two paths inside a service
belongs to NIM-768. On an instance with no `aclfile` directive `ACL SAVE` cannot succeed
at all: declare `persist: false`.

## user.present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `name` | string | required | The ACL username **being managed**. Carries no whitespace. Not the same as `username`, which is who the step authenticates as. |
| `perms` | string | required | The FULL ACL rule string, Redis directives verbatim (`~app:* +@read +@write`). **Total, not additive** — whatever is not here is not granted, because the apply resets the user first. A credential-bearing token (`>pass`, `<pass`, `#hash`, `!hash`) is refused, as are `reset`, `on`/`off`, and `nopass`/`resetpass` beside a declared `user_password` — all case-insensitively. `nopass`/`resetpass` alone are allowed. |
| `state` | string | optional (default `on`) | `on` or `off`. **Quote it in YAML**: unquoted `on`/`off` parse as booleans (YAML 1.1), and a boolean is *refused* rather than coerced — coercing `off` would silently land as the default `on`. |
| `user_password` | string (secret) | optional | Password OF THE MANAGED USER. vault-ref; sent as `#<sha256hex>`, never plaintext. Omit to keep the credential the user already holds; on a user that does not exist yet, omitting it creates one that cannot authenticate until you declare a password or put `nopass` in `perms`. |
| `persist` | bool | optional (default `true`) | Run `ACL SAVE` after a CHANGING `SETUSER`. Not run on a no-op. `false` on an instance with no `aclfile`. A non-boolean is *refused*, not coerced: the fallback would be "write it to disk". With it on, the apply first asks `CONFIG GET aclfile` and refuses without mutating anything if there is none. That pre-flight never fails the apply on its own account: `CONFIG` is `@admin` while the ACL commands are not, so a connection that cannot read the config (NOPERM, `rename-command`) proceeds exactly as it did before the pre-flight existed. |
| `password` | string (secret) | optional | Redis password for the **connection**. vault-ref, keeper resolves to Apply (see "Password"). Masked; **not** passed into the arguments of ACL commands. |
| `username` | string | optional | ACL-username for `AUTH` — who the step authenticates **as** (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `ACL SETUSER`. Inert — the ACL is server-wide — but declared because it is part of the shared connect path. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

**Output**: `name` and the declared `state`. The `ACL LIST` lines the change was diffed
against carry password hashes and go nowhere near Output — the same rule `acl.reloaded`
follows. Without dry-run preview.

## user.absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Redis address: `host:port` (TCP) or `unix:/path`. |
| `name` | string | required | The ACL username to remove. Carries no whitespace and no NUL, and is not `default` — refused by this module in `Apply` as well as `Validate`. Redis refuses `default` too, but a safety borrowed from the server is one the next release can change. To disable `default` instead, use `user.present` with `state: "off"` — **only once another account can authenticate**: on an instance whose only user is `default`, that succeeds, is saved, and leaves every new connection on `NOAUTH`, recoverable only by editing the aclfile out of band. |
| `persist` | bool | optional (default `true`) | Run `ACL SAVE` after a removal that actually happened. Not run on the no-op — including the case where `DELUSER` answers `0` because another actor removed the user between the probe and the command. A non-boolean is *refused*, not coerced. |
| `password` | string (secret) | optional | Redis password for the **connection**. Masked; not passed into ACL command arguments. |
| `username` | string | optional | ACL-username for `AUTH` (if not default-user). |
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `ACL DELUSER`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

**Output**: `name`. Without dry-run preview.

## cluster — the object

Manages the Redis cluster **entirely via go-redis** (no `redis-cli`/shell).
The operation is address level 3 — one action per operation (NIM-766; there is no `action:` param). Implemented:

- day-1/day-2 over **your** cluster: `created` (build from scratch), `node-added`
(attach one node), `node-removed` (withdraw one node), `resharded` (transfer
N slots master→master);
- three steps **live migration between clusters** (old → new, without downtime):
`external-joined` (infuse new cluster-mode nodes with replicas of the old cluster 1:1),
`failed-over` (promotion of new replicas to the master via graceful failover),
`external-forgotten` (discard old nodes).

> **★ Idempotency.** `created`/`node-added`/`node-removed`/`external-joined`/
> `failed-over`/`external-forgotten` **idempotent** - re-apply on
> converged input gives `changed=false` (no-op), it is safe to keep them in converge.
> **`resharded` - NO.** This is an imperative **exec-style** day-2 operation (without `unless`):
> applying again will shift **more** `slots` slots from `from` to `to`. The operator is calling
> reshard **explicitly**, exactly as many times as transfers are needed; reshard **not** part
> convergence loop.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `password` | string (secret) | optional | See "Password". Applies when connecting to **each** node (`created`) / to `new_node`+`seed`+`master` (`node-added`) / to `node`+`seed`+remaining masters (`node-removed`) / to `from`+`to` (`resharded`) / to the new `nodes`+`source_nodes` (`external-joined`/`failed-over`/`external-forgotten` - **common** password of the old and new cluster). |
| `username` | string | optional | ACL-username for `AUTH`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters, applied to **every** node connection incl. `source_nodes` - one cluster PKI (see "TLS connection"). |

> **★ No `addr`/`db` here.** `cluster` is the one state without a single instance:
> it connects to each node of `nodes`/`seed`/`source_nodes` in turn, so the
> manifest declares neither `addr` nor `db` for it - only the auth+TLS keys above.

### cluster.created — params

Assembles a cluster from the `nodes` set: `CLUSTER MEET` (gossip) → `CLUSTER ADDSLOTS`
masters → `CLUSTER REPLICATE` replicas. Idempotent - repeat call to already
formed cluster (`cluster_state:ok`, composition coincided, 16384 slots
covered) gives `changed=false`, no-op.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `nodes` | map | required (`created`) | Nodes: map stable-key (SID/name) → `{ addr: "host:port" }` or `{ ip: "10.0.0.1", port: 6379 }`. **The keys are sorted** - they determine the master/replica layout. `addr` - for connection, `ip`+`port` - for `CLUSTER MEET` (gossip operates `ip:port`, not a DNS name). |
| `replicas_per_shard` | int | optional (default `0`) | Replica to the shard. `shards = len(nodes) / (1 + replicas_per_shard)`; `len(nodes)` must be divided by the size of the shard without a remainder. |
| `topology` | list | optional | **Explicit** shard layout, a list of shards of `nodes` keys: `[[master-sid, replica-sid, …], …]`. Replaces the deterministic key-sort layout when the operator needs specific pairings (anti-affinity across racks/AZ). Every key of `nodes` must appear **exactly once**; the first entry of a shard is its master. With `replicas_per_shard > 0` every shard must be exactly that size; without it, shard sizes are free. |

**Deterministic layout.** `nodes` keys are sorted; first `shards`
nodes are masters, the rest are round-robin replicas to the masters (`replica j →
master j%shards`). 16384 slots are divided equally between masters; remainder
(`16384 % shards`) is allocated one slot to the first masters. One and the same
same input `nodes` always gives the same topology and the same ranges
slots.

**Gossip convergence** - limited retry (not infinite loop): after `MEET`
the plugin waits until `CLUSTER NODES` shows all nodes, and only then sends
`ADDSLOTS`/`REPLICATE`. If the limit is not met - `failed`.

### cluster.node-added — params

Attaches **one** new node to an already formed cluster (`CLUSTER MEET`
via `seed` → `CLUSTER REPLICATE` to master with `role: replica` or empty
master at `role: master`). Idempotent (`CLUSTER NODES`): node is already in the cluster →
`changed=false`, no-op. `role: master` adds **empty** master with no slots -
moving slots is a separate `resharded` (node-added does not move slots).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `new_node` | map | required (`node-added`) | Attached node: `{ addr: "host:port" }` or `{ ip, port }`. `addr` - for connection (`REPLICATE` is executed on it), `ip`+`port` - for `CLUSTER MEET`. |
| `seed` | map | required (`node-added`) | Any existing cluster node is a contact for `MEET` and a source for `CLUSTER NODES` (idempotency): `{ addr: "host:port" }` or `{ ip, port }`. |
| `role` | string | optional (default `replica`) | Newbie role: `replica` (`CLUSTER REPLICATE` to `master` or least loaded) or `master` (empty master with no slots). |
| `master` | map | optional (`role: replica`) | Master, whose replica the newcomer will be: `{ addr: "host:port" }` or `{ ip, port }`. Not specified → the plugin selects the master with the smallest number of replicas (balancing as `redis-cli` without `--cluster-master-id`). |

### cluster.node-removed — params

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
> non-atomic, no rollback, same as `resharded`. If the operation fails **after**
> `SETSLOT IMPORTING`/`MIGRATING`, but **before** the final `SETSLOT NODE` (break at
> `MIGRATE`, error in the middle of multi-batch slot `> 100` keys), the slot will get stuck in
> suspended IMPORTING(target)/MIGRATING(source), already migrated slots -
> **remain** carried over, `FORGET` is **not** executed yet, apply will return `failed`,
> cluster is in an inconsistent intermediate state. Recovery **manual**:
> check `CLUSTER NODES`, on stuck slots or `CLUSTER SETSLOT <slot>
> STABLE`, or repeat `node-removed` (it will migrate the remainder and complete
> `FORGET`). This is the **conscious semantics** of an imperative operation (like `redis-cli
> --cluster`), **not a bug**.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `node` | map | required (`node-removed`) | Output node: `{ addr: "host:port" }` or `{ ip, port }`. If this is a master with slots, the slots are first migrated to the remaining masters. |
| `seed` | map | required (`node-removed`) | Any existing cluster node is a contact for `CLUSTER NODES` (topology + idempotency) and a source of a list of nodes for `FORGET`: `{ addr: "host:port" }` or `{ ip, port }`. |

### cluster.resharded — params

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
> <slot> STABLE`, or repeat `resharded` (it will finish the remainder). This is **conscious
> semantics** of the imperative operation (like `redis-cli --cluster`), **not a bug**.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `from` | map | required (`resharded`) | Master-**source** of slots: `{ addr: "host:port" }` or `{ ip, port }`. Must be a master in the cluster and own `>= slots` slots. |
| `to` | map | required (`resharded`) | Master-**recipient** of slots: `{ addr: "host:port" }` or `{ ip, port }`. Must be a master in the cluster and different from `from`. |
| `slots` | int | required (`resharded`) | How many slots to transfer (`>= 1`). The first `slots` source slots are taken in ascending order. **Not idempotent** - repeated apply will transfer another `slots`. |

### cluster — live migration between clusters (`external-joined` → `failed-over` → `external-forgotten`)

Three steps transfer the load from the **old** cluster-mode cluster to the **new** without
downtime (mirror manual `redis-cli --cluster add-node`/`--cluster failover`/
`--cluster del-node`, entirely via go-redis). Both clusters are on the **same** network under
**single** password/TLS (operator aligns `new == old` before migration). Order
strict: `external-joined` (replicas are catching up with the old masters) → `failed-over`
(promotion, slots are moving to new ones) → `external-forgotten` (old nodes are forgotten).
Implementation - [`migrate.go`](../../../examples/module/redis/migrate.go).

**`external-joined`** — merge new nodes into the old cluster and make each one a replica
old master **1:1**: connection to `source_nodes` → `CLUSTER NODES` old cluster
→ mapping new-node↔old-master (nodes by key `nodes`, masters by first slot)
→ `CLUSTER MEET` old-seed + waitConverge + `CLUSTER REPLICATE` on **each** new
node. **Fail-fast**: number of old masters `!= shards_dest` → 1:1 not possible
(runtime-assert, `shards_source` is not visible in the render phase). Idempotent: the node is already
replica of the desired master → no-op.

**`failed-over`** — promote new nodes (replicas of old masters after
`external-joined`) to the master via **graceful** `CLUSTER FAILOVER`. **First
sync-gate**: on **each** new node `INFO replication` `master_link_status == up`
(the replica caught up with the old master) - at least one didn't catch up → **error before the first
failover** (early failover loses its tail). Then on each node graceful `CLUSTER
FAILOVER` (no arguments: master stops recording + sends tail, lossless) →
poll to `role==master` with slots. **Fail-closed**: graceful did not meet the limit →
**error, WITHOUT** escalation to `FORCE`/`TAKEOVER` (aka split-brain). Idempotent:
node is already master → no-op.

**`external-forgotten`** — throw out old nodes: connect to `source_nodes` → `CLUSTER
NODES` from the old cluster → **all** old node IDs (masters **and** replicas) → `CLUSTER
FORGET <old-id>` on **every** new node. **Without** migration of slots (new ones already have slots
masters after `failed-over`). Idempotent: the old id is no longer known to the node
(`Unknown node`) → swallowed as no-op. Decommission of the old hosts themselves (stop
redis, cleaning `nodes.conf`) is **outside** of this operation.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `nodes` | map | required | **New** nodes (map stable-key → `{ addr }` / `{ ip, port }`). `external-joined` maps them 1:1 to old masters (keys ↔ masters), `failed-over` promotes, `external-forgotten` performs on every `CLUSTER FORGET`. |
| `source_nodes` | list | required | Seed nodes of the **old** cluster (list `host:port`). They go through in order - the first one to respond, `CLUSTER NODES`, sets the topology. Same password/TLS as new nodes. |
| `shards_dest` | int | required (`external-joined`) | Expected number of destination shards (`>= 1`). It must match BOTH the number of new nodes (`nodes`) AND the number of masters of the old cluster - otherwise 1:1 mapping is impossible (fail-fast; assert in Apply, because `shards_source` is visible only in the live topology). |

## replica.present — params

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
| `db` | int | optional (default `0`) | Database number (`SELECT`) before `REPLICAOF`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS parameters of the **plugin's own** connection to this instance (see "TLS connection"). Unrelated to `master_tls*` below, which describe the **replication link** to the source. |

### replica.present — params (`source_external`)

Binding to an **external** master (someone else's incarnation / migration), and not to your own host
incarnations are the first step in migrating data from old Redis. At `source_external: true`:
(1) self-guard `addr == master_addr` **disabled** (the external address is obviously not yours);
(2) `masterauth` is taken from `master_password` (not `password`); (3) `masteruser` - from
`master_username`. Further migration proceeds through `replica.offset-synced` (catching up) → `replica.detached`
(promotion). The TLS of the outgoing replication link to the source is enabled by `master_tls`.

> **★ TLS link to the source requires render on disk.** `master_tls: true` includes
> `CONFIG SET tls-replication yes` **to** `REPLICAOF`, but CA/cert/key of Redis source
> reads **from disk along the way**, not inline. Plugin files **not** writes: `master_tls_ca`/
> `master_tls_cert`/`master_tls_key` should put scenario replicas on disk (via
> `core.file.rendered`) and specify paths via `instance.configured` (`tls-ca-cert-file`/
> `tls-cert-file`/`tls-key-file`) **before** the `replica.present` step. Otherwise server-cert verification
> source with a handshake will fail. The plugin does not convert PEM values ​​themselves into paths.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `source_external` | bool | optional (default `false`) | `master_addr` points to external master (migration). `true` - enables `master_*` details and removes self-guard. |
| `master_password` | string (secret) | optional | External master source password (`CONFIG SET masterauth`). vault-ref, keeper resolves to Apply. Masked. Empty → `masterauth` is not set. |
| `master_username` | string | optional | ACL-username of the external source for replication (`CONFIG SET masteruser`). |
| `master_tls` | bool | optional (default `false`) | The source receives the replica over TLS. `true` → `CONFIG SET tls-replication yes` to `REPLICAOF`. Requires rendering the source CA/cert to disk (see sidebar). |
| `master_tls_ca` | string (secret, PEM) | optional | PEM CA of an external source (checking its server-cert on the replication link). Masked. Placed on disk by render, path via `config`-state. |
| `master_tls_cert` / `master_tls_key` | string (secret, PEM) | optional | PEM client-cert/key replicas for mTLS on a replication link to the source (only together). Masked. Used as `tls-cert-file`/`tls-key-file` by render, not by plugin. |

**Decision on the three `master_tls_*` PEMs: they stay declared, and the manifest
does not try to say "consumed by render" (NIM-229).** The contract reads oddly on
purpose — the module accepts a CA it never applies — so the alternatives were
weighed rather than defaulted into:

- *A new manifest field marking a param as render-consumed* is **not
  implementable**. `manifest.yaml` parses under `yaml.Strict()`, so an unknown key
  is a decode error, and plugin discovery skips the whole slot on one — an older
  Soul would not see the param as ungated, it would lose the entire module
  (`module.not_found`). This is the same wall [NIM-204](../../adr/0076-engine-compat-window.md)
  hit looking for an opt-in flag, and it applies to any new manifest key.
- *Moving them out of the module's params into a render-step contract* would fail
  every scenario that passes them today with `module.unknown_param`, since
  NIM-204 made a plugin's manifest gate its input.
- *Leaving them undeclared* was the pre-NIM-206 state and is what NIM-206 fixed.

So the honest contract is: declared, masked, and described here and in the
manifest as read by **render**, not by the module. The plugin's only act under
`master_tls: true` is `CONFIG SET tls-replication yes`; the PEMs reach Redis as
files on disk, placed by `core.file.rendered` and pointed at through `instance.configured`.
Pinned by `TestManifestStatesDeclareWhatTheyAccept` in `manifest_test.go`, whose
per-state roster keeps the exception from spreading in silence.

## replica.detached — params

Detaches the instance from the master via `REPLICAOF NO ONE` (go-redis), promoting it to
independent master. **The final** step of migration from an external source is after
`replica.offset-synced` confirmed catch-up (`caught_up == true`). Idempotent (`INFO
replication`): instance already `role == master` → `changed=false`, no-op (safe for
I will repeat). Implementation - [`detach.go`](../../../examples/module/redis/detach.go).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | The address of **this** instance is `host:port` or `unix:/path`. |
| `password` | string (secret) | optional | Redis password. vault-ref, keeper resolves to Apply. Masked; `REPLICAOF` is not passed to arguments (only goes to the connection). |
| `username` | string | optional | ACL-username for the connection (if not default-user). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS connection parameters (see "TLS connection"). |

**Output**: `changed` (bool) — whether the instance was promoted; `previous_master`
(line `host:port`) - the previous master for auditing (`""`, if the instance was already master
or `master_host`/`master_port` fields were missing).

## sentinel.monitored — params

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
| `db` | int | optional (default `0`) | **Must be `0`, and anything else is refused** — a Sentinel serves no keyspace and answers `SELECT` with an error, so a non-zero value makes the connection unopenable rather than merely pointless. Declared rather than dropped: the key is part of the shared connect path every scenario passes, and since [NIM-204](../../adr/0076-engine-compat-window.md) an undeclared param fails the task. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | General TLS parameters of the connection **to Sentinel** (see "TLS connection"). The TLS of the master Sentinel monitors is a `sentinel.conf` directive and travels in `config`. |

## Password (IS-invariant ADR-010)

Plugin capacity - **only `network_outbound`**, `vault_access` **no**:
the password comes already resolved from Keeper. In operator-input
(scenario/destiny) the password is set by vault-ref via CEL `${ vault(...) }`;
keeper-side render phase resolves it **to** `Apply` and passes it to the plugin
plaintext value (ADR-012 - Soul/Vault client plugin does not work). In the manifesto
`password` is marked `secret: true` + `pattern: "^vault:.*"` - this forces
vault-ref at the input and masking in logs/trace/UI.

`user.present` is the one state whose secret is not only a connect credential:
`user_password` is the password OF THE MANAGED USER, and `ACL SETUSER` takes a password
as an argument by design. It is sent as `#<sha256hex>` and never as plaintext, so the
invariant below holds for it in the same form as for every other secret here — the
plaintext appears in no argument, event, output or error. See "user — the object".

Code invariant (checked by L0): `params["password"]` (and `auth_pass` in `sentinel.monitored`,
`master_password` to `replica.present source_external`, `source_password` to `replica.offset-synced`,
`user_password` to `user.present`)
**never** get into `ApplyEvent.Message`/`.Output`, error text and
stderr. Connection errors are sanitized (`redactError` cuts out the substring
password - during the second connection, `replica.offset-synced` edits **both** passwords); conclusion
of the commands themselves (`result`) is the server's response, not the operator's secret. In `sentinel.monitored`
Output carries only the **names** of the applied actions (`sentinel_monitor`/`sentinel_set`/…),
not their secret values; `replica.present` sets `masterauth` as argument to `CONFIG SET`
(Redis is needed for synchronization), but it does not write it to events.

## TLS connection (`redis_tls_*` parameters)

All objects accept **general** TLS connection parameters.
TLS is disabled by default (plaintext, back-compat); at `tls: true` the plugin connects
to Redis over TLS. Two states have a **second** set of TLS parameters for the external link:
`replica.offset-synced` — `source_tls`/`source_tls_ca`/`source_tls_cert`/`source_tls_key`/
`source_tls_skip_verify` (connection to an external source, its own PKI),
`replica source_external` — `master_tls`/`master_tls_ca`/`master_tls_cert`/`master_tls_key`
(outgoing replication link of the replica to the source; see sidebar in ["replica `source_external`"](#replica--params-source_external)).

> **★ Every state declares the set in its own `input:`, and that is what counts.**
> Param-level strictness (ADR-0076) checks a task against
> `spec.states.<state>.input` and nothing else - a promise in this document or in
> the manifest header is not a declaration. Four states used to omit the set
> entirely, so an operator's typo in a TLS key passed unnoticed (NIM-206). The
> guard is a table test over all eleven states in
> [`manifest_test.go`](../../../examples/module/redis/manifest_test.go).
> `cluster` is the one exception on `addr`/`db`: it has no single instance.

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

`tls` is a **boolean**, and `tls: "true"` written as a string is refused rather than
read as `false` — see ["Parameter types are refused, not coerced"](#parameter-types-are-refused-not-coerced)
below, which is the rule for every parameter here and not only this one.

## Parameter types are refused, not coerced

A parameter whose value is not of its declared type **fails the step**, addressed by
name (`params.tls: must be a boolean (true/false), got a string`). Nothing is guessed
and nothing falls back to a default.

This is not a style preference. `tls: "true"` used to read as `false`, so the
connection — the password with it — went out in **plaintext**, and the step reported
`reconciled` with no diagnostic at all: an author's typo became a silent leak instead
of a refusal, because `false` is the insecure side of that parameter. `db: "7"` read
as database `0` the same way. Fixed in **NIM-778**.

Two consequences worth knowing before you write a scenario:

- **Quote nothing that is not a string.** `tls: true`, `db: 7`, `persist: false` —
  bare. A CEL cell that is *entirely* one `${ … }` keeps its native type (ADR-010),
  so `tls: "${ vars.redis_tls }"` is fine **when the var is a real boolean**; if the
  var is the string `"true"`, the step now fails instead of connecting in the clear.
  That is the point — but it means a var file with `redis_tls: "true"` in it needs
  fixing, and it will announce itself the first time the step runs.
- **The check is not `soul-lint`'s.** The Keeper's static `checkParamType` returns
  clean on a `${ … }` cell, and the runtime calls `Apply`, not `Validate` — so this
  artifact makes the refusal itself, in `Apply`, before it opens a socket. (Teaching
  the linter to catch it earlier is NIM-779; this does not depend on it.)

The rule is derived from what each state declares in its own `input:`, so a
parameter added later inherits it without anyone remembering to. Guarded by a table
test that walks every object, every action and every declared `bool`/`int` parameter
in [`params_test.go`](../../../examples/module/redis/params_test.go). Keys
**nested inside** a map-typed parameter are not declared individually and are checked
by hand where they are read: `monitor.port`, `monitor.quorum`, and the `port` of a
cluster node spec.

> **★ One node-spec case changed beyond the type check.** A cluster node written as
> `{addr: "10.0.0.1:6379", port: "6379"}` — a valid `addr` *next to* a wrong-typed
> `port` — used to succeed: `port` coerced to `0` and the spec fell through to the
> `addr` branch. It now fails. The spec is contradictory either way, and silently
> resolving it in favour of the key the author did *not* get wrong is the guess this
> whole section removes. Write `port: 6379` unquoted, or drop the key.

Beyond parsing: a connection that asks for TLS and cannot get it **fails**. It does
not retry in the clear — guarded against a listener that speaks no TLS, asserting the
password never reaches the wire.

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
connects. Scenario of service `redis` in step `redis.cluster.created`
forwards `tls_cert`/`tls_key` to the plugin (resolved from the same
`vault(vars.tls_cert_ref/tls_key_ref)`, same as server PEM redis.conf) -
plugin builds mTLS pair (`tls.go`: client-cert is added when **both** are specified
`tls_cert`+`tls_key`). Without mutual-bus these parameters do not interfere with handshake
(only used if the server requested them). `cert`/`key` host-invariant
(one per cluster) → correctly go through `apply.input`.

**Anti-downgrade (IS).** Plugin connections in scenario `redis` are gated to
`vars.tls_enable`, **not** `tls_only`: when `tls_enable: true` the plugin connects
via TLS even when the plain port is still open (`tls_only: false`). Otherwise the AUTH password is gone
over the plaintext network (plaintext-downgrade). Connection port - `tls_port` when
`tls_enable`, otherwise plain `6379`.

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` - TCP/unix/**TLS** connection to Redis
(for `cluster` - connection to each node from `nodes`, for cluster live migration - more
and to `source_nodes` of the old cluster; for `replica.offset-synced` - **second** connection to
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
  module: redis.instance.configured
  params:
    addr: "127.0.0.1:6379"
    # The password is resolved by keeper-side via vault() in the render phase (ADR-012):
    # it's the value that goes into the plugin, not the link.
    password: "${ vault('secret/redis/' + incarnation.id + '#password') }"
    config: "${ state.redis_config }"

# Raw command (probe): changed=false by default.
- name: Ping redis
  module: redis.command.run
  register: pong
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.id + '#password') }"
    args: ["PING"]
```

Migration from external Redis (three steps, health-gate by `caught_up`):

```yaml
# 1. Link the local instance with a replica to the EXTERNAL master.
- name: Replicate from external source
  module: redis.replica.present
  params:
    addr: "127.0.0.1:6379"
    master_addr: "${ input.source_addr }"
    source_external: true
    master_password: "${ vault('secret/redis/legacy#password') }"

# 2. Wait for the complete data catch-up (not just a live link).
- name: Wait until caught up with source
  module: redis.replica.offset-synced
  register: sync
  until: register.self.caught_up == true
  retry: { attempts: 60, delay: 5 }
  params:
    addr: "127.0.0.1:6379"
    source_addr: "${ input.source_addr }"
    password: "${ vault('secret/redis/' + incarnation.id + '#password') }"
    source_password: "${ vault('secret/redis/legacy#password') }"

# 3. Untie and promote to a separate master (migration final).
- name: Detach and promote to master
  module: redis.replica.detached
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.id + '#password') }"
```

## Tests

- **L0 command/config/acl**
  ([`impl_test.go`](../../../examples/module/redis/impl_test.go)):
fake `redisConn` + fake `ApplyEvent`-stream. Covers `Validate` (empty
addr/args/config, acl requires addr, unimplemented state), Apply happy-path
command/config, unix-socket-parsing, `changed`-semantics, numeric stringification
values, `CONFIG REWRITE`; **startup-only-denilist** (`instance.configured` skips
`port`/`dir`/`aclfile`/`cluster-enabled`/`loadmodule` - neither `CONFIG GET` nor `SET`
are not called by it, `skipped`/`skippedCount` in Output are correct; all-startup-only →
`changed=false`, none `SET`); **acl** (`ACL LOAD` is sent between `ACL LIST`
before/after, `changed=true` on diff / `false` on match, error `LOAD`/`LIST` →
`failed`); and **IS-invariant** - the password does not leak into events or arguments
commands, nor in a sanitized connection error.
- **L0 probe (pinged/role/replica-synced)**
  ([`probe_test.go`](../../../examples/module/redis/probe_test.go)):
fake `redisConn`. Covers `Validate` (empty `addr`); `instance.pinged` happy-path
(`PING` → `Output.result == 'PONG'`, `changed=false`), error `PING` → `failed`;
  `instance.role-probed` happy-path (`INFO replication` → `Output.role` = `master`/`slave`,
`changed=false`), `INFO replication` without field `role` → `failed`; `replica.synced`
(`master_link_status: up` → `synced=true`; field missing → `synced=false` with
reason); **IS-invariant** (password does not flow into events/sanitized error
connection).
- **L0 offset-synced**
  ([`offset_synced_test.go`](../../../examples/module/redis/offset_synced_test.go)):
fake `redisConn` (own + external source). Covers `Validate` (requires `addr` +
`source_addr`, rejects negative `lag_threshold`); `caught_up=true` when
catching up; `lag > threshold` / `lag <= threshold`; `master_sync_in_progress` → not
caught_up; `link down` → not caught_up; lack of offset → not caught_up; opt.
`DBSIZE`-checksum and `skip_checksum`; that the **second** connection uses `source_*`-
details and `source_tls` (regardless of its TLS); **IS-invariant** (neither yours nor
source password does not leak - incl. when the second connection fails).
- **L0 cluster**
  ([`cluster_test.go`](../../../examples/module/redis/cluster_test.go)):
fake-fleet of nodes by addr. Covers `Validate` (empty `nodes`, non-`created`
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
  ([`cluster_reshard_l3c_test.go`](../../../examples/module/redis/cluster_reshard_l3c_test.go)):
e2e-live vs real cluster (build-tag `e2e_live` + `t.Skip` to
harness-entity "live redis cluster"). TODO-invariant: writing keys to slots
source (incl. whitespace+TTL), one imperative reshard, real check
slot owner changes + lossless keys + TTL + convergence `DBSIZE`.
Compiled in a gate, it really doesn't run without a live cluster.
- **L0 replica**
  ([`replica_test.go`](../../../examples/module/redis/replica_test.go)):
fake `redisConn` with scripted `INFO replication`. Covers `Validate`
(no `master_addr`); `REPLICAOF` + `masterauth` BEFORE it; idempotency (already
replica of the desired master → no-op); `addr == master_addr` → master-guard no-op
(no commands); empty password → `masterauth` is not set; `source_external`
(self-guard removed, `masterauth`/`masteruser` from `master_*`, `tls-replication yes`
at `master_tls`); **IS-invariant** (neither `password` nor `master_password` flows
in events/sanitized connection error).
- **L0 detached**
  ([`detach_test.go`](../../../examples/module/redis/detach_test.go)):
fake `redisConn` with scripted `INFO replication`. Covers `Validate` (empty
`addr`); slave → `REPLICAOF NO ONE` + `changed=true` + `previous_master` in Output;
already master → no-op (`changed=false`, no commands); error `INFO` → `failed`;
**IS-invariant** (the password does not flow into the sanitized connection error).
- **L0 cluster live migration (join-external/failover-takeover/forget-external)**
  ([`migrate_test.go`](../../../examples/module/redis/migrate_test.go)
  + [`migrate_failover_test.go`](../../../examples/module/redis/migrate_failover_test.go)):
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
- **L0 user (present/absent)**
  ([`user_test.go`](../../../examples/module/redis/user_test.go)):
fake `redisConn` with a scripted `ACL LIST` (before/after) and a per-verb failure, so
"`SETUSER` failed" and "`ACL SAVE` failed" are distinguishable — the difference IS the
persistence invariant. Covers `Validate` (empty `addr`/`name`/`perms`, a name carrying
whitespace, a state that is neither `on` nor `off`, and `absent` on `default`);
create + `ACL SAVE`; idempotent no-op that does **not** save; Redis's normalization of
the rules not reading as a change; `persist: false`; `absent` on a user the instance
does not have sending no command; a name that is merely a substring of a live user not
matching. Guards rather than coverage: **`reset`** opens the vector and a rule outside
the declaration does not survive it; the credential is **hashed and present** (a
dropped password is not a fix); the live `#hash`/`nopass` is **carried past the reset**;
a **failed `SETUSER` persists nothing** (the NIM-624 shape, mirrored); a **boolean
`state` is refused** by both `Validate` and `Apply` (the YAML 1.1 `on`/`off` trap);
**IS-invariant** (neither the connection password nor the managed user's password
reaches arguments, events or a sanitized connect error). Ten mutations of the real code
were run against these guards and all ten turned them red.
  A second set answers an independent review of this object, and each one drives the
real `Apply` because that is the only entry point a runner uses: an **empty `perms`**
is refused and reaches neither `SETUSER` nor `ACL SAVE` (unchecked, `reset` would strip
every permission the live user holds and persist it); an **empty `name`** is refused
(real Redis accepts one and writes a nameless entry no operator can then address); a
**credential-bearing token in `perms`** is refused *and the refusal does not echo it*
(the message goes to the same logs the rule protects); **`nopass` stays legal** and
reaches the instance last; a **non-boolean `persist`** is refused rather than coerced
toward writing to disk, on **both** states; **`absent` on `default`** never reaches
`DELUSER`; and a **`DELUSER` answering `0`** — another actor got there first — is a
no-op that does not rewrite the aclfile.
  A third set came from a second review, which broke the first two guards on a live
Redis: the perms keywords are refused in **any case** (`RESET` bypassed an exact
comparison and cost a live user its declared perms, state and password while the step
reported success); **`on`/`off` in `perms`** is refused because it overrides `state` and
makes `Output.state` lie; and **`nopass`/`resetpass` beside a declared
`user_password`** is refused because Redis takes the last directive, which left an
account authenticating with any string at all.
  A fourth came from a third review, which broke the second round's fix the same way:
a **NUL byte** restores every one of those harms, because `strcasecmp` stops there and
`EqualFold` does not — so a NUL in `perms` or in `name` is refused, on both. That review
also produced the **`CONFIG GET aclfile` pre-flight**: a persisting apply against an
instance with no aclfile used to land the `SETUSER`, fail on `ACL SAVE`, and report
failure — after which a re-run reported a no-op, so no run ever reported the change.
Both no-aclfile refusals assert that nothing was mutated, and `persist: false` against
the same instance still applies.
  A fifth came from a fourth review, and two of the three were regressions the
pre-flight itself introduced: a connection that may run `ACL SETUSER` but **not**
`CONFIG GET` still applies (`configErr` on the fake — the shape no earlier fake could
express); an `absent` **no-op on an instance with no aclfile** is green, not a hard
failure; and a **credential inside a selector** (`(>secret +get)`) is refused, where a
prefix test on byte 0 let it reach Redis, which quoted the plaintext straight back into
the failure message.
- **L0 sentinel**
  ([`sentinel_test.go`](../../../examples/module/redis/sentinel_test.go)):
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
cd examples/module/redis
GOWORK=off go build ./...   # binary redis (gitignored)
GOWORK=off go test ./...    # L0
```

Module - separate go.mod from `replace` to core (`../../../proto/plugin`,
`../../../sdk`); going standalone, not included in `go.work` (convention
`examples/module/`).

## See also

- [README.md](../../README.md) - module directory (directory status).
- [examples/service/redis/](../../../examples/service/redis/) - redis service:
  scenario `create` (standalone/cluster/sentinel), `add_node`, `remove_node`,
`reshard` (day-2, **NOT idempotent**) and day-2 hot-reload `update_config`
  (→ `instance.configured`), `add_user` (→ `acl.reloaded`), `rotate_tls` (→ `command.run`,
force re-read SSL_CTX), `migrate_cluster` (→ the `cluster` live migration + `replica.present`
  `source_external` + `replica.offset-synced`), `detach_source` (→ `replica.detached` + `replica.offset-synced`)
call the states of this plugin.
- [examples/destiny/redis/](../../../examples/destiny/redis/) —
mode-agnostic per-host brick (install + render `redis.conf` + systemd).
- [ADR-012](../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, password
reaches the value.
- [ADR-031 Scry](../../adr/0031-scry-drift.md) — default-deny on dry_run without
  `PlanReadSafe`.
- [templating.md](../../templating.md) - secret masking (§7.4).
