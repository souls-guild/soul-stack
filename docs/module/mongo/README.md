# mongo

> ★ **Re-laid-out by NIM-769.** Every address on this page is
> `mongo.<object>.<action>` ([address rule](../../naming-rules.md#the-discipline-binding-the-three-levels)):
> level 1 is the registration alias, level 2 the **object** this plugin manages, level 3 the
> **action**. The grouping level `community.*` is gone — it named where the plugin came from,
> not what it manages. The old form `community.mongo.<state>` resolves nowhere; a cluster that
> still registers the artifact as `community` must change the alias in
> `keeper.yml::plugins.*[].name`, which is a config edit and nothing more.
>
> `params.state` went with it. `present`/`absent` are two ACTIONS now, so a service that
> chose between them per item — the corpus loop over `input.users` did — filters the loop
> instead of passing the verb as a parameter. The service's own `input.users[].state` is
> unchanged: it is still the operator's declaration, and the scenario is what turns it
> into an address.

> ★ **Made manageable by NIM-805.** Five objects were added beside the first three:
> `replicaset`, `role`, `collection`, `index`, `database`. The three that were here are
> unchanged apart from one thing they gained with everybody else — a **parameter of the
> wrong type is now REFUSED rather than coerced**, on `Validate` AND on `Apply`
> (NIM-800, the mongo half of the redis artifact's NIM-778). That is a TIGHTENING:
> `tls: "true"` written as a string used to fall through to `false` and send the admin
> password to `mongod` in plaintext while reporting success, and it is now an error
> naming the parameter. A definition that relied on the coercion starts failing, which is
> the point.

MAIN interface to **live MongoDB**: the service scenario orchestrates order/targeting/health-gate, and the plugin
performs **one** operation on one `mongod` instance. Custom plugin
`kind: soul_module` serving **eight objects** — `collection`, `command`, `database`,
`index`, `instance`, `replicaset`, `role`, `user` — from one
binary, `soul-mod-mongo`; the artifact carries no name of its own (NIM-377), so level 1 is
whatever alias the operator registers it under. Implementation -
[`examples/module/soul-mod-mongo/`](../../../examples/module/soul-mod-mongo/)
(`object.go` - the object tables and the dispatch, `bundle.go` - the eight `module.Def`s
the schema document is generated from, `obj_*.go` - one file per object's declaration,
`impl.go` - the shared driver + instance.pinged/command.run, `user.go` -
user.present/user.absent (createUser/dropUser + localhost-exception bootstrap),
`replicaset.go` / `role.go` / `collection.go` / `index.go` / `database.go` — one file per
object added by NIM-805, `params.go` — the parameter-type refusal (NIM-800), `compare.go` —
the declared-vs-live diff every idempotent action runs on, `input.go` — the connection
block the NIM-805 objects declare from one place,
`conn.go`/`tls.go` — connect, `helpers.go` - structpb/secret-helpers).

Backend — [`go.mongodb.org/mongo-driver`](https://go.mongodb.org/mongo-driver):
the plugin itself connects to `mongod` via TCP (`host:port`, usually `127.0.0.1:27017`).
Work is carried out **through the driver, and not `core.exec` + `mongosh`** (the password in `argv` is
IS risk, fragile output parsing; the same path `shell -> plugin` took
[`redis`](../redis/README.md)). Compliance with core modules
(`core.pkg`/`core.file`/`core.service`/`core.sysctl`) - for everything that is NOT
mongo-specific(installation, render `mongod.conf`, systemd, host-tuning); himself
MongoDB runtime is this plugin.

> **★ Scope (updated by NIM-805).** **Fifteen actions across eight objects**: standalone
> (one `mongod`, `security.authorization: enabled`, admin via localhost-exception) AND
> **replica-set** — initiate, complete to the declared membership, grow, shrink,
> reconfigure. Still outside (documented when it appears in the code):
> **sharded** (`mongos`/config/shard + [Choir](../../naming-rules.md)) — deferred with a
> reason in **NIM-820**: it needs a `mongos` and config-server replica sets that exist
> nowhere in this tree, and `addShard` stands on `replicaset`, which did not exist before
> this. Also outside: `keyFile` (intra-cluster SCRAM authentication) and day-2 on a
> user's password/roles (**NIM-821** — the SCRAM verifier CAN be reproduced and compared,
> see the note in `user.present` below). TLS: `mongod` in the corpus service runs plain,
> and the `tls*` parameters are declared on every action for forward-compat.

## Without dry-run preview (consciously)

The plugin remains at `module.BaseModule` - **doesn't** implement `PlanReadSafe`
([ADR-031](../../adr/0031-scry-drift.md)) and `ErrandReadSafe`
([ADR-033](../../adr/0033-errand.md)). This is a conscious choice (parallel
`redis`): on `dry_run` host (Soul) applies **default-deny** - task
gets the honest "drift not supported" rather than the false "no drift".

## Objects and actions

Eight objects (`schema.json::modules[]`), **fifteen** actions between them. The object is
address level 2 — what is being managed — and the action is level 3: the state it is left
in, or, on the one non-stateful object, the single verb naming the operation.

| Object | Actions | What it manages |
|---|---|---|
| `collection` | `present`, `absent` | One collection with its options. Creating one is also what brings its DATABASE into being — MongoDB has no command that creates a database. |
| `command` | `run` | An arbitrary MongoDB command, run as given. Non-stateful, so level 3 is the verb form (`core.exec.run`'s shape). It takes exactly one verb — a second operation would be a second object. |
| `database` | `absent` | One database, DROPPED. There is deliberately no `present` — see below; that is an answer, not a gap. |
| `index` | `present`, `absent` | One index on one collection. Its own object and not a list inside `collection.present` — an index has its own lifecycle and CANNOT be modified in place. |
| `instance` | `pinged` | A live `mongod` as a whole: is it answering. |
| `replicaset` | `initiated`, `member-added`, `member-removed`, `reconfigured` | A replica set. The four differ in what they are ALLOWED to do, and that difference is the safety property. |
| `role` | `present`, `absent` | One user-defined role. The object of this artifact that converges on a real STRUCTURAL diff, because `rolesInfo` returns the whole grant. |
| `user` | `present`, `absent` | One MongoDB user. Users live in `admin.system.users`, not in a config file (unlike the redis `users.acl` a destiny renders), so the subject is reached directly through `createUser`/`dropUser`. |

| Action | Destination | `changed` |
|---|---|---|
| `instance.pinged` | Health-probe via go-mongo-driver `Ping` (primary). Read-only. Replaces idiom `command.run { ping: 1 }` - health-gate in scripts (`retry`/`until`/`failed_when` by `register.self.ok`). | `false` **constructive** (probe, not change). |
| `user.present` | `createUser` (if the user is missing). Idempotent by `usersInfo`. ★ the first admin is created via **localhost-exception** (see below). | `true` on a real create; `false` (no-op) when the user already exists. |
| `user.absent` | `dropUser` (if the user is present). Idempotent by `usersInfo`. NO localhost-exception fallback — removal requires privileges. | `true` on a real drop; `false` (no-op) when the user is already gone. |
| `command.run` | Raw `db.runCommand` (imperative verb-action, precedent `redis.command.run`/`core.exec.run`). | `false` default (probe); `changed: true` in params - for actually mutating commands (the operator is responsible for idempotency). |
| `replicaset.initiated` | `replSetGetConfig` decides: NotYetInitialized → `replSetInitiate`; the declared membership already live → no-op; part of it live → `replSetReconfig` adding ONLY the missing members. | `true` on an initiate or a completion; `false` (no-op) on a set that already is what was declared. |
| `replicaset.member-added` | The live config plus one member, `version+1`, sent to the PRIMARY. Idempotent by the config. A host already in the set with DIFFERENT declared attributes is a **failure**, not a no-op. | `true` on a real join; `false` when the host is already a member **as declared**. |
| `replicaset.member-removed` | The live config minus one member, `version+1`, sent to the PRIMARY. | `true` on a real eviction; `false` when the host is not a member. |
| `replicaset.reconfigured` | A PATCH over EXISTING members: only the attributes named in params are laid over the live member documents. The one action that may change a live member. | `true` when something really moved; `false` when every declared attribute already matches. |
| `role.present` | `rolesInfo` + `showPrivileges` decides: absent → `createRole`; grant differs → `updateRole` (which REPLACES it); identical → no-op. | `true` on a create or a real update; `false` on a grant that already matches. |
| `role.absent` | `dropRole` (if the role is present). Idempotent by `rolesInfo`. | `true` on a real drop; `false` when the role is already gone. |
| `collection.present` | `listCollections` decides: absent → `create` with the declared options; MUTABLE option differs → `collMod`; IMMUTABLE option differs → **failure naming the field**. | `true` on a create or a `collMod`; `false` when the declared options already hold. |
| `collection.absent` | `drop` (if the collection is present). ★ destroys its documents and indexes. | `true` on a real drop; `false` when it is already gone. |
| `index.present` | `listIndexes` decides: absent → `createIndexes`; `hidden`, or `expire_after_seconds` on an index that ALREADY has a TTL, differs → `collMod index`; a different KEY, an immutable option, or a TTL being ADDED → **failure**. | `true` on a create or a `collMod`; `false` when the index already matches. |
| `index.absent` | `dropIndexes` (if the index is present). A collection that does not exist is a no-op, not a failure. | `true` on a real drop; `false` when it is already gone. |
| `database.absent` | `dropDatabase`, with `changed` from TWO sources — the server's `dropped` field and a `listDatabases` pre-read; either says yes → changed. ★ destroys everything in it; `admin`/`local`/`config` are refused. | `true` on a real drop; `false` when the database is not there. |

## instance.pinged — params

Health-probe via go-mongo-driver `Ping` (primary). **Read-only**,
`changed=false` constructive. `Output.ok == true` - condition for health-gate
(`until: register.self.ok == true`); in the script `create` is used as gate
"mongod replied" **to** bootstrap admin. Error `Ping` (mongod has not risen yet,
unavailable) → `failed`.

> `Ping` itself does not require authorization, so `pinged` **before** creation
> `default_admin` occurs via localhost-exception (empty admin-DB). `password`
> is not specified in the pilot script at this step.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Address `mongod`: `host:port` (usually `127.0.0.1:27017`). |
| `username` | string | optional | ACL-username for AUTH (if not anonymous). |
| `password` | string (secret) | optional | MongoDB password. vault-ref in operator-input, keeper resolves to Apply (see "Password"). Masked; is not transmitted to `Ping` (goes to the connection). |
| `auth_db` | string | optional (default `admin`) | `authenticationDatabase`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | TLS connection parameters (see "TLS connection"). PILOT: mongo in plain - not specified. |

**Output**: `ok` (bool) - `true` if `Ping` is successful.

## user.present / user.absent — params

`createUser`/`dropUser` over live `mongod` entirely via go-mongo-driver.
Idempotent by `usersInfo(name)`: `present` + user exists → no-op (changing the
password or roles of an existing user is day-2 and is **NIM-821** — the SCRAM verifier
`usersInfo` returns with `showCredentials` CAN be reproduced from a candidate password
and compared, which is the answer to NIM-383 §2; it is not done here); `present` + no →
`createUser` (`changed=true`); `absent` + is → `dropUser` (`changed=true`);
`absent` + no → no-op.

Each action declares only what it reads, which is what the split bought: `roles` and
`user_password` are `present`'s and are **refused** on `absent` (`module.unknown_param`),
where under the single-address form they were declared and silently ignored.

> **★★ Localhost-exception bootstrap** (mongo mechanics, similar to redis
> `default_admin` bootstrap). `mongod` with `security.authorization: enabled`
> allows connection **without auth** only through loopback (localhost) and only for now
> there is not a single user in the admin database. The first admin (`default_admin`) is created exactly
> like this: connection with auth is not yet possible (there is no user). Mechanics - **inside the plugin**
> (`user.go`), not in render: render passes `addr`+`username`+`password`, plugin
> decides the auth path **based on the live state** (parallel to the redis plugin,
> decider on `INFO`/`CONFIG GET`). With `present`: (1) tries connection with auth +
> cheap `usersInfo`-ping; (2) auth crashes `Unauthorized`(13)/`AuthenticationFailed`(18)
> - this is expected for the first admin → fallback to **no-auth** localhost connection;
> (3) `createUser` of the first admin goes through no-auth. Once admin is created,
> exception is closed - further connections are made with auth. `absent`-path fallback
> **doesn't** do (removing a user requires rights - this is not a bootstrap case).
> Output carries `used_localhost`/`bootstrap_admin` (whether the no-auth path worked).

| Param | On | Type | Required/default | Meaning |
|---|---|---|---|---|
| `addr` | both | string | required | Address `mongod`: `host:port`. |
| `name` | both | string | required | The name of the MongoDB user to be created/deleted. This is the SUBJECT of the step, not the user to authenticate as (that is `username`). |
| `database` | both | string | optional (default `admin`) | The database in which the user is created (login/roles-context). Roles without an explicit `db` inherit it. `absent` runs `dropUser` in the same context. |
| `roles` | `present` | list | optional* | User roles - array `{role, db}` (**exact mongo model**: user = a set of named roles, each in a specific database). `db` without a value inherits `database`. *Must be non-empty (a user without roles is meaningless - check in Apply, where an EXISTING user is a no-op that needs none). |
| `password` | both | string (secret) | optional | Password **ADMIN CONNECT** (a user is created under `username`). vault-ref, keeper resolves to Apply. Goes to connection cradle, does not get into events. ★ This is **not** the password of the user being created - except for the bootstrap of the first admin, where the admin creates himself. |
| `user_password` | `present` | string (secret) | optional | Password of the **CREATED** user (`pwd` of document `createUser`). vault-ref, keeper resolves to Apply. Separated from `password` (connect-auth admin). Not set → fallback to `password` (bootstrap of the first admin). Masked. |
| `username` | both | string | optional | ACL-username of the AUTH connection (administrator under which the user is created). With bootstrap of the first admin, auth is not yet possible → localhost-exception. |
| `auth_db` | both | string | optional (default `admin`) | `authenticationDatabase` connection. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | both | — | optional | TLS connection parameters. PILOT: mongo in plain - not specified. |

A param marked `present` is **not declared** on `absent`, so passing it there is refused
(`module.unknown_param`) rather than ignored — that is the contract the action split makes
checkable (NIM-206, NIM-769).

**Output**: `present` (bool) — user state after the operation; `changed` (bool) —
was there a real create/drop; `used_localhost`/`bootstrap_admin` (bool) - worked
whether no-auth localhost path (only on `user.present`).

## command.run — params

Raw `db.runCommand` to MongoDB (imperative verb-state, use case
`redis.command.run`/`core.exec.run`). Default `changed=false` (probe);
operator is responsible for idempotency. For pilot - single-field command
(`{ serverStatus: 1 }`, `{ collStats: "events" }`).

> **WARNING** The output of the command is the mongo response, **not** managed by the plugin
> secret: masks [ADR-010](../../adr/0010-templating.md) it is **not** covered
> (Output carries only the `ok` flag, but the command error text is the server response). **Not**
> run through `command` read commands that return secrets (`usersInfo` with
> `showCredentials`) - their result would have been published in clear text; for this -
> specialized state, whose output declares the secret fields ([ADR-0083](../../adr/0083-declared-secret-state-fields.md) §8).
> `params.password` itself is masked
> and does not go into the command arguments (it only goes into the connection).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `addr` | string | required | Address `mongod`: `host:port`. |
| `db` | string | optional (default `admin`) | Target database for `runCommand`. |
| `command` | map | required | bson command document (first/only key - command name): `{ serverStatus: 1 }`. For pilot - single-field. |
| `username` | string | optional | ACL-username for AUTH. |
| `password` | string (secret) | optional | MongoDB password. vault-ref, keeper resolves to Apply. Masked; is not passed to command arguments (goes to the connection). |
| `auth_db` | string | optional (default `admin`) | `authenticationDatabase`. |
| `changed` | bool | optional (default `false`) | Mark the result `changed=true` (for actually mutating commands). Default `false` (probe semantics). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | TLS connection parameters. PILOT: mongo in plain - not specified. |

**Output**: `ok` (bool) — response success flag (`{ ok: 1 }` → `true`).

## replicaset — params

Four actions over `replSetGetConfig` / `replSetGetStatus` / `replSetInitiate` /
`replSetReconfig`, entirely through go-mongo-driver.

> **★★ THE SAFETY INVARIANT.** `replSetInitiate` on an already-initiated set and
> `replSetReconfig` on a live one are different operations with different costs, and the
> second can force a primary re-election and break writes in flight. So the object never
> guesses which it is in: it **ASKS** and branches on the answer — the same shape
> `redis.cluster.created` has, which reads `CLUSTER INFO`/`CLUSTER NODES` before deciding
> whether to build, complete or no-op. **An external probe plus a `when:` over it is
> deliberately NOT the design here**: a second guard of the same invariant drifts away
> from the first, and it cannot make the two distinctions below.
>
> **And the stronger half: no reconfig is ever ASSEMBLED FROM PARAMS.** Every one of them
> is the LIVE document from `replSetGetConfig`, mutated minimally, with `version+1`. So
> `settings`, `protocolVersion`, `term` and everything else this artifact does not model
> ride through untouched — an operator's out-of-band `settings` survives a member being
> added. And the `_id` of an existing member is never reassigned; a new member takes
> `max(_id)+1`, not an index into the array, because a member removed earlier leaves a
> hole and filling it hands a new host a retired identity.

**Two "not initiated" answers, and only one is ours.** A `mongod` started WITHOUT
`replication.replSetName` refuses these commands with **NoReplicationEnabled (76)**; one
started with it but never initiated refuses with **NotYetInitialized (94)**. The first is a
config edit and a restart away and the plugin says so by name — a `when: not initiated`
guard reads both as "go ahead" and drives `replSetInitiate` into an instance that can only
refuse it.

**`initiated` is ADDITIVE ONLY.** A member the live config holds and `params.members` does
not is **refused**, not dropped (`member-removed` does that deliberately) — a silent drop
is how a set loses its majority. An existing member whose attributes differ is
**refused**, not rewritten (`reconfigured` does that deliberately) — a priority change can
force an election, and this step reads as assembly. Reporting `changed=false` on a drifted
set would be the third option, and it would be a lie about convergence.

**`member-added` compares the declared attributes too**, and refuses a difference the same
way. Matching on the host alone would make `changed=false` a fact about the HOSTNAME rather
than about the declaration: a member joined here as a hidden non-voter and later given
`priority: 1` out of band would go on reporting "already in the set" while it had become
able to win an election.

**Bootstrap.** With `security.authorization: enabled` a set is initiated BEFORE the first
admin exists, because the localhost exception holds only while the admin DB has no users.
So `initiated` takes the same connection path `user.present` takes — auth first, no-auth
loopback on an auth failure. The three day-2 actions do **not**: an auth failure there is a
failure.

**The primary hop.** `replSetReconfig` only runs on the primary, and `replSetGetStatus`
names it by its **config host**, which is routable between members and not necessarily from
the host this plugin runs on — the same split redis has between a node's `addr` (dial) and
its `ip:port` (gossip). A member therefore declares `host` (what goes into the config) and
an optional `addr` (what we dial); `primary_addr` is the override for the day-2 actions,
which are given one member rather than the whole set.

| Param | On | Type | Required/default | Meaning |
|---|---|---|---|---|
| `name` | `initiated` | string | required | The set name — `_id` of the config; must equal the `mongod`'s own `replication.replSetName`. A LIVE set whose name differs is **refused rather than renamed**: renaming a replica set is not an operation. |
| `members` | `initiated`, `reconfigured` | map | required | Stable key (SID/name) → member spec. Keys are **SORTED**, and that determines the `_id` assigned on a fresh initiate — so the same input yields the same config. |
| `member` | `member-added` | map | required | The one member being joined (same spec). |
| `host` | `member-removed` | string | required | The `members[].host` being evicted, as it appears in the config. A host written without a port is compared as `:27017`, which is how `mongod` stores it. |
| `wait_primary_seconds` | all | int | optional (default `60`) | How long to wait for a PRIMARY. `0` reads once and does not poll. **Running out is a FAILURE**: a set without a primary accepts no writes, so reporting the step reconciled would be false. |
| `primary_addr` | all | string | optional | Where to reach the primary from THIS host, if its config `host` is not routable here. |
| `addr` / `username` / `password` / `auth_db` / `tls*` | all | — | see below | Connection. Identical on every action of this artifact. |

**Member spec** (`members[<key>]` and `member`). An attribute that is **not written is not
touched**: `mongod`'s own default applies on a member being created, and the live value
survives on one being changed.

| Key | Type | Meaning |
|---|---|---|
| `host` | string, **required** | `host:port` as the OTHER MEMBERS see it — what goes into the replica-set config. |
| `addr` | string | `host:port` to DIAL from this host, when it differs from `host`. |
| `priority` | number (default 1) | `0` means it can never be elected. |
| `votes` | int, `0` or `1` (default 1) | |
| `arbiter_only` | bool | Requires `votes` 1, and `priority` 0 — which `mongod` DEFAULTS for an arbiter, so `{host, arbiter_only: true}` alone is legal (that is why `rs.addArb()` takes no priority argument). |
| `hidden` | bool | Requires `priority` 0. |
| `build_indexes` | bool (default true) | `false` requires `priority` 0. |
| `secondary_delay_secs` | int | `> 0` requires `priority` 0. (The 5.0 spelling; `slaveDelay` is not served.) |
| `tags` | map string→string | Compared order-insensitively — `mongod` returns the keys in whatever order it stored them. |

> ★ **A key this table does not list is REFUSED, not ignored.** The engine's
> `unknown_param` stops at the outer `members` map — nothing declares what is INSIDE a
> member spec — so this is the NIM-800 rule carried one level down by hand. It is not
> hypothetical: three of these attributes are spelled differently here from the `mongod`
> config an author is reading (`arbiter_only`/`arbiterOnly`,
> `build_indexes`/`buildIndexes`, `secondary_delay_secs`/`secondaryDelaySecs`), and a
> dropped `arbiterOnly: true` joins a full data-bearing secondary — one that initial-syncs
> the whole dataset — where an arbiter was declared, and reports it reconciled.

Every rule in that last column is refused by **`Validate`**, not met mid-run
(NIM-786) — as are `votes` outside 0..1, more than 7 voting members, more than 50 members,
two entries on one host, and a set in which nothing can be elected. On `reconfigured` the
rules spanning an attribute the operator did NOT name are left to the server instead:
`hidden: true` with no `priority` beside it is legal there, because the live config may
already hold `priority: 0` and refusing it would refuse input `Apply` accepts.

**Output**, per action. `initiated`: `set`, `members` (count), `members_added`,
`initiated` (bool — was this the `replSetInitiate`), `primary`, `version`,
`used_localhost` (whether the no-auth bootstrap path fired, on EITHER dial).
`member-added` / `member-removed`: `host`, `members`, `primary`, `version`.
`reconfigured`: `members`, `members_changed`, `primary`, `version`.

Every action publishes the SAME keys on its no-op path as on its changed one —
otherwise a downstream `${ register.rs.primary }` would break on exactly the run that
changed nothing.

## role — params

`createRole` / `updateRole` / `dropRole`, decided by comparing the LIVE grant from
`rolesInfo` + `showPrivileges` with the declared one. This is the object of the artifact
with a real structural diff, so `changed=false` here is a fact about the instance and not
about what the plugin did last time.

The comparison runs on a **canonical form** — actions sorted and de-duplicated, resources
in one spelling, the whole list sorted — because `mongod` returns them in an order of its
own and a raw comparison would report a change on **every** apply.

★ `updateRole` **REPLACES** the grant rather than adding to it. That is what makes
`present` a converge: a privilege the role picked up out of band is gone afterwards.

| Param | On | Type | Required/default | Meaning |
|---|---|---|---|---|
| `name` | both | string | required | The role being managed — the SUBJECT of the step, not the role of the account it connects as. A built-in `mongod` name is refused (see below). |
| `database` | both | string | optional (default `admin`) | The DB the role lives in — a role name is unique only within one. A privilege resource and an inherited role naming no `db` of their own inherit this one. |
| `privileges` | `present` | list | optional* | `{ resource, actions }`. `resource` names **exactly one** of `{ db, collection }` (`collection` may be `""` for the whole DB), `{ cluster: true }` or `{ any_resource: true }` — naming two is **refused rather than resolved**, since they grant very different things. `actions` is a non-empty list of `mongod` action names. |
| `roles` | `present` | list | optional* | The roles this one INHERITS: `{ role, db }`. |
| `addr` / `username` / `password` / `auth_db` / `tls*` | both | — | see below | Connection. |

\* not both empty: a role that confers nothing is not a state to declare, and that is
refused in `Validate` and in `Apply`.

Two grants on the **same resource** are refused: `mongod` MERGES them before storing, so
`[{events,[find]}, {events,[insert]}]` reads back as one `{events,[find,insert]}` — two
declared against one live, never equal, `updateRole` on every apply forever. Put a
resource's actions in a single entry. The same rule applies to `roles`: a `{role, db}`
inherited twice is refused, for the same reason.

**Built-in roles are refused on both actions**, twice over: a static list in `Validate`
(so an author is told before the run) and the live `isBuiltin` flag in `Apply` (which
cannot go stale as `mongod` adds names). The static list is scoped by DATABASE:
`read`/`readWrite`/`dbAdmin`/`dbOwner`/`userAdmin` exist everywhere, while `root`,
`backup`, `clusterAdmin` and the `*AnyDatabase` set exist only in `admin` — so `root` in
`appdb` is a perfectly legal user-defined name, and refusing it would refuse input `Apply`
accepts, which is NIM-786 pointing the other way.

> ★ **`role.absent` is a privilege change for every account that held the role.** `mongod`
> removes it from them, and the access it granted goes with it.

**Output**: `database`, `name`, `present`, `privileges` (count), `roles` (count).

## collection — params

`create` / `collMod` / `drop`, decided by `listCollections`.

> ★★ **THE MUTABLE/IMMUTABLE SPLIT IS THE CONTENT OF THIS OBJECT.** Some options can be
> changed on a live collection and some cannot, because changing them means rebuilding it
> and therefore moving the data. A declared IMMUTABLE option that differs from the live one
> is a **failure naming the field** — not a silent no-op, and not a drop-and-recreate. The
> only way to apply it is to lose the collection's data, and that is an operator's decision,
> not a converge step's.

| Param | Type | Mutability | Meaning |
|---|---|---|---|
| `database` | string, **required** | — | **NOT defaulted to `admin`** (unlike `user.database`): a collection in the admin database is almost never intended, and a default there would create one silently. |
| `name` | string, **required** | — | The collection. |
| `capped` / `size` / `max` | bool / int / int | **immutable here** | Capped collection: fixed size in BYTES, optional document cap. `size` is compared as `mongod` STORES it — a value at or below 4096 becomes exactly 4096, anything above is raised to a multiple of 256 — so a declared `1000000` matches a stored `1000192`, and a declared `1024` matches a stored `4096`, instead of failing the second apply of the step that created it. Resizing a live capped collection IS possible from MongoDB 6.0 (`collMod` with `cappedSize`), but through `mongo.command.run`: the operation is server-version dependent, and silently doing nothing on an older server would be worse than refusing. |
| `collation` | map | **immutable** | e.g. `{ locale: "en", strength: 2 }`. |
| `timeseries` | map | **immutable** | e.g. `{ timeField: "ts", metaField: "meta" }`. A time-series collection cannot be converted to or from one. |
| `clustered_index` | map | **immutable** | e.g. `{ key: { _id: 1 }, unique: true }`. |
| `validator` | map | mutable (`collMod`) | Document validation predicate, e.g. `{ $jsonSchema: … }`. |
| `validation_level` | string `off\|strict\|moderate` | mutable (`collMod`) | |
| `validation_action` | string `error\|warn` | mutable (`collMod`) | |

An option that is **not declared** is not compared and not sent: `mongod`'s default applies
on a create, the live value survives a `collMod`.

A declared `collation: { locale: "simple" }` matches a collection **or index** that stores
NO collation: "simple" is the binary collator, i.e. no collation at all. (Against one that
does store a collation, it is still a difference.) The same holds for `max: 0` and
`size: 0`, which `mongod` omits rather than stores.

`collation`, `timeseries` and `clustered_index` are otherwise compared **on the keys you
declared only**, because `mongod` stores them with its own defaults filled in — a declared
`collation: { locale: "en" }` comes back as a document with eight more keys, and comparing
those whole would report a difference on every apply, on an IMMUTABLE option, where the
answer is to fail the step. `validator` is compared EXACTLY: one that lost a clause out of
band has really changed.

The live KIND must be the one that was **declared**: a plain collection where `timeseries`
was declared, a time-series one where it was not, or a **view** either way, is refused. It
is a mismatch that is wrong, not the time-series form itself — `listCollections` reports
what `timeseries` created as `type: "timeseries"`, so a flat "must be an ordinary
collection" rule would fail the SECOND apply of a step that succeeded on the first, on a
collection this artifact made itself.

> ★ **`collection.absent` destroys the collection's documents and its indexes.** There is
> no confirmation parameter — this artifact has none anywhere, and a flag an author always
> sets is not a gate. What guards it is a scenario deciding when to run it. (Same shape as
> `redis.cluster.node-removed`, which migrates slots and FORGETs without one.)

A declared **default is not a default here**: an option you do not write is not compared
and not sent, so the schema declares no `default:` on `capped` — a default nothing
materializes would tell an author that omitting it asserts `false`, when omitting it
asserts nothing at all.

A probe reply this plugin cannot PARSE is a **failure**, not "the collection is not
there". An empty batch is the genuine absence; anything unreadable would otherwise make
`absent` report a no-op about a collection still present.

**Output**: `database`, `name`, `present`, `database_created` — the last one says whether
THIS step is what brought the database into being.

## index — params

`createIndexes` / `collMod index` / `dropIndexes`, decided by `listIndexes`.

> ★★ **THE KEY IS A LIST, NOT A MAP.** An index on `{ user_id: 1, created_at: -1 }` is a
> **different index** from one on `{ created_at: -1, user_id: 1 }`, with different query
> plans. A YAML map reaches a plugin as an unordered structure — the same limitation
> `command.run` documents when it says only single-field commands are reliable — so a key
> declared as a map would build a different index from one run to the next. It is compared
> order-sensitively for the same reason.

> ★★ **AN INDEX CANNOT BE MODIFIED.** Changing what it is ON — its key, its uniqueness, its
> partial filter, its collation — is not an operation `mongod` offers. It means dropping the
> index and building a new one, and during that build the queries that relied on it have no
> index at all. So a declared key or immutable option that differs is a **failure naming the
> field**, and the operator drops it deliberately (`index.absent`) if that is what they meant.

| Param | Type | Mutability | Meaning |
|---|---|---|---|
| `database` / `collection` | string, **required** | — | Where the index lives. |
| `name` | string, **required** | — | How an index is ADDRESSED. `_id_` is refused on both actions: `mongod` creates and maintains that one itself and will not drop it. |
| `keys` | list, **required** | **immutable** | Ordered list of `{ field, order }`. `order` is `1` (ascending, the default), `-1`, or an index type as a string (`"2dsphere"`, `"2d"`, `"hashed"`, …). A value that is neither is **refused, not defaulted**: an ascending index where a descending one was meant is a query plan nobody notices until it is slow. |
| `unique` / `sparse` | bool | **immutable** | |
| `partial_filter_expression` | map | **immutable** | e.g. `{ status: { $eq: "active" } }`. |
| `collation` | map | **immutable** | Compared on the declared keys only (server-normalized). A declared `{ locale: "simple" }` matches an index that stores no collation — "simple" is the binary collator, which `mongod` does not store. |
| `expire_after_seconds` | int | mutable (`collMod`) **on an index that already has a TTL** | TTL: seconds a document survives past the date in its indexed field. ★ **This is where a TTL belongs — on the INDEX, not on the collection.** ADDING a TTL to an index that has none is not an operation `mongod` offers — `collMod` answers `InvalidOptions` — so that is refused like any other rebuild. `0` is a real value (expire at the indexed date itself), not "unset". |
| `hidden` | bool | mutable (`collMod`) | Hide from the query planner while keeping it maintained — how an index is tested for removal without paying to rebuild it if the answer is no. |

> ★ **Two index kinds this object deliberately does NOT serve**, because it could not tell
> a converged one from a drifted one and would ask you to drop an index it had just built:
>
> - **`order: "text"` is refused.** `mongod` stores a text index under a REWRITTEN key —
>   `key` becomes `{_fts: "text", _ftsx: 1}` and the declared fields move into `weights` —
>   so a declared `{title: "text"}` can never match what `listIndexes` reads back.
> - **`wildcard_projection` is not a parameter.** `mongod` normalizes the projection it
>   stores (dotted paths expanded into nested documents, `1`/`0` turned into
>   `true`/`false`), so `{"payload.secret": 0}` reads back as `{payload: {secret: false}}`.
>   A wildcard index WITHOUT a projection (`$**`) is served normally.
>
> Both are served by `mongo.command.run { createIndexes: … }` in the meantime. A promise
> that cannot be kept is worse than an absent feature.

Adding a TTL to an index that has none is **refused**, not attempted: `collMod` answers
`InvalidOptions` to it, so it is a rebuild like any other. Changing an existing TTL is a
`collMod`. `expire_after_seconds: 0` is a real value, not "unset".

`index.absent` on a collection that does not exist is a **no-op**, not a failure
(`NamespaceNotFound` is "no such index") — without that the teardown half of a scenario
reds whenever the collection is already gone.

**Output**: `database`, `collection`, `name`, `present`.

## database — params (`absent`, and only `absent`)

`changed` comes from TWO sources and a no-op needs both silent: `dropDatabase`'s own
`dropped` field, and a `listDatabases` pre-read on `admin`. Neither alone is enough —
a missing `dropped` is also what an unknown reply shape looks like, and reporting
`changed=false` after destroying a database would leave a downstream
`when: register.*.changed` unfired; while `listDatabases` alone has a privilege hole
(since 4.0.5 `mongod` silently applies `authorizedDatabases`, so an under-privileged
caller sees a well-formed EMPTY list). **The step therefore issues a `listDatabases`
on `admin` before every drop, and a read it cannot parse is a hard failure.**

> ★★ **WHY THERE IS NO `present`, AND WHY THAT IS AN ANSWER RATHER THAN A GAP.** MongoDB
> has no command that creates a database: one exists once it holds a collection. So a
> `database.present` could only do one of two things — nothing at all, a step reporting
> success having had no observable effect, which is the shape of lie this artifact exists
> to avoid; or secretly create a collection, inventing state the operator never declared.
>
> What actually creates a database is **`mongo.collection.present`**, which is MongoDB's own
> semantics rather than a workaround, and it reports `database_created` so a scenario can
> see when it happened. (This is the answer to NIM-383 §3.)

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | The database being dropped — the SUBJECT of the step; the connection's own auth database is `auth_db`. |
| `addr` / `username` / `password` / `auth_db` / `tls*` | — | see below | Connection. |

> ★ **This destroys every collection, index and document in the database.** `admin`,
> `local` and `config` are **refused** on both the `Validate` and the `Apply` path: they are
> what `mongod` itself runs on, and dropping one destroys the users, roles and replication
> bookkeeping the instance needs. That refusal is the one gate in this artifact that is more
> than a description in a state's prose, because its blast radius is the server rather than
> the operator's data.

**Output**: `name`, `present`.

## Parameter types are REFUSED, not coerced (NIM-800)

Every object carries its own declaration (`obj_*.go`) at runtime, and both `Validate` and
`Apply` check each param's value against the declared type before anything opens a socket.
A value of the wrong type is an **error naming the parameter and the expected type**.

This is the mongo half of the redis artifact's NIM-778, and it exists because of a live
leak: `tls: "true"` written as a **string** fell through `boolOrDefault` to `false`, the
connection to `mongod` went out in **plaintext with the admin password in it**, and the
step reported success. The direction of the fallback is what made it a leak — `false` is
the insecure side of that parameter.

Nothing upstream catches it. The runtime calls `Apply`, not `Validate`, and the Keeper's
static `checkParamType` returns nil on a `${…}` cell, so `tls: "${ vars.mongo_tls }"` over
a string var lints clean. **The plugin is the last place that can say no.**

Three things the check deliberately does not do: an UNDECLARED key is left to the engine
(`module.unknown_param`, NIM-204); an ABSENT key is what a default is for; and a NULL —
a YAML key written with nothing after it — reads as unset, not as a wrong type. The rule
is derived from the declaration rather than written at each read site, so an object added
later inherits it without anyone remembering to (and `TestEveryObjectCarriesItsDecl` fails
one that does not).

The nested specs carry the rule the rest of the way: `members[<key>].priority: "0"`
falling back to the default `1` would make a member the operator pinned out of elections
able to win one.

## Password (IS-invariant ADR-010)

Plugin capacity - **only `network_outbound`**, `vault_access` **no**:
The password comes already resolved from Keeper. In operator-input (scenario/destiny)
the password is set by vault-ref via CEL `${ vault(...) }`; keeper-side render phase
resolves it **to** `Apply` and passes the plaintext value to the plugin
([ADR-012](../../adr/0012-keeper-soul-grpc.md) - Soul/Vault client plugin is not
pulls). In the manifest, `password`/`user_password`/`tls_*` are marked `secret: true` +
`pattern: "^vault:.*"` - this forces vault-ref on input and masking in
logs/trace/UI.

Code invariant (checked by L0): neither `params.password` nor `params.user_password`
**never** get into `ApplyEvent.Message`/`.Output`, error text and
stderr. The password of the created user goes only to the `pwd` field of the document
`createUser`; Connect password - only in Connect cradle. Connection/command errors
are sanitized (`redactError` to `helpers.go` cuts out the substring of each secret -
for the user path, **both** are edited: `user_password` and connect-`password`).
The password of the created user is in Vault according to convention
`secret/mongo/<incarnation>/users/<name>#password` (redis/dragonfly symmetry).

## TLS connection (forward-compat; pilot - plain)

All fifteen actions declare a **common** set of TLS connection parameters
(`tls`/`tls_ca`/`tls_cert`/`tls_key`/`tls_skip_verify`), but **in pilot mongo
works in plain mode** - these parameters are not set (mongo TLS on port 27017
via `net.tls.mode` - a separate slice). Parameters are declared for forward-compat.

| Parameter | Type | Default | Destination |
|---|---|---|---|
| `tls` | bool | `false` | Connect to `mongod` via TLS. |
| `tls_ca` | string (secret, PEM) | — | CA certificate for server verification (RootCAs). |
| `tls_cert` | string (secret, PEM) | — | Client certificate for mTLS (optional, **only together** with `tls_key`). |
| `tls_key` | string (secret, PEM) | — | Client key for mTLS (optional, **only together** with `tls_cert`). |
| `tls_skip_verify` | bool | `false` | **EXPLICIT opt-out** checking server certificate (default secure). |

Security model (insecure = explicit opt-out, default secure): at `tls: true`
the default plugin **checks** the server certificate; disable - only explicit
`tls_skip_verify: true`. PEM comes **entirely** to params (keeper-side resolves from
Vault via `${ vault(...) }`), the plugin does not support its Vault access; masking - by
key name (`shared/audit`).

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` — TCP/TLS connection to `mongod`.
**Without** `vault_access` (password and PEM resolve Keeper), **without**
`exec_subprocess` / `fs_write_root` (plugin does not start subprocesses - working
goes through go-mongo-driver, not through `mongosh` - and does not write to FS).
- `side_effects: [{ service: mongod }]`, declared PER OBJECT (NIM-769) rather than
  artifact-wide - all eight objects work on a live `mongod`, and an operator approving
  one sees what that one touches.

## Example call from scenario

```yaml
# Health-gate: wait for mongod to respond to ping BEFORE bootstrap admin.
- name: Wait for mongod to answer ping
  module: mongo.instance.pinged
  retry:
    count: 15
    delay: 3s
    until: "register.self.ok == true"
  failed_when: "register.self.ok != true"
  params:
    addr: "127.0.0.1:27017"

# Bootstrap of the first admin (default_admin) via localhost-exception:
# admin-DB is empty → the plugin makes a fallback to a no-auth localhost connection.
- name: Bootstrap the default_admin user (localhost-exception)
  module: mongo.user.present
  params:
    addr:     "127.0.0.1:27017"
    username: default_admin
    # The password is resolved by keeper-side via vault() in the render phase (ADR-012):
    # it's the value that goes into the plugin, not the link.
    password: "${ vault('secret/mongo/' + incarnation.id + '/users/default_admin#password') }"
    name:     default_admin
    database: admin
    roles:    [{ role: root, db: admin }]

# The removal half. `state` is not a param any more — the verb is the address, so a
# service choosing per item filters its loop (examples/service/mongo/scenario/create).
- name: Remove a MongoDB user
  module: mongo.user.absent
  params:
    addr:     "127.0.0.1:27017"
    username: default_admin
    password: "${ vault('secret/mongo/' + incarnation.id + '/users/default_admin#password') }"
    name:     appuser
    database: appdb
```

Replica set, roles, collection and index (NIM-805). Note what is NOT here: no probe task
and no `when:` gating the initiate. The object reads `replSetGetConfig` itself, so the same
task is the build on a fresh set, the completion on a half-built one and a no-op on a
converged one.

```yaml
# Build the set, or complete it, or do nothing — decided by the instance, not by us.
# Runs over the localhost-exception: on a fresh host the first admin does not exist yet.
- name: Replica set rs0
  module: mongo.replicaset.initiated
  run_once: true
  params:
    addr: "127.0.0.1:27017"
    name: rs0
    members:
      # The map KEY is the stable identity; `host` is what the members call each other,
      # `addr` is what THIS host dials when the two differ.
      "01": { host: "mongo-1.internal:27017", addr: "10.0.0.1:27017" }
      "02": { host: "mongo-2.internal:27017", addr: "10.0.0.2:27017" }
      "03": { host: "mongo-3.internal:27017", addr: "10.0.0.3:27017", priority: 0, hidden: true, votes: 0 }
    wait_primary_seconds: 90

# A user-defined role. updateRole REPLACES the grant, so this converges rather than
# accumulates: a privilege added out of band is gone after the next apply.
- name: Role app_writer
  module: mongo.role.present
  run_once: true
  params:
    addr:     "127.0.0.1:27017"
    username: default_admin
    password: "${ vault('secret/mongo/' + incarnation.id + '/users/default_admin#password') }"
    name:     app_writer
    database: appdb
    privileges:
      - resource: { db: appdb, collection: "" }
        actions:  [find, insert, update]
    roles:
      - { role: read, db: appdb }

# Creating the collection is also what creates `appdb` — Output.database_created says so.
- name: Collection appdb.events
  module: mongo.collection.present
  run_once: true
  params:
    addr:              "127.0.0.1:27017"
    username:          default_admin
    password:          "${ vault('secret/mongo/' + incarnation.id + '/users/default_admin#password') }"
    database:          appdb
    name:              events
    validation_level:  strict
    validator:
      $jsonSchema:
        bsonType: object
        required: [created_at]

# The index is its OWN task, and `keys` is an ORDERED LIST: a map would not keep
# { user_id, created_at } in that order, and the order is what the index IS.
- name: Index appdb.events by_user_time
  module: mongo.index.present
  run_once: true
  params:
    addr:       "127.0.0.1:27017"
    username:   default_admin
    password:   "${ vault('secret/mongo/' + incarnation.id + '/users/default_admin#password') }"
    database:   appdb
    collection: events
    name:       by_user_time
    keys:
      - { field: user_id,    order: 1 }
      - { field: created_at, order: -1 }
    expire_after_seconds: 604800
```

## Tests

- **L0 dispatcher (instance/command)**
  ([`impl_test.go`](../../../examples/module/soul-mod-mongo/impl_test.go)):
fake `mongoConn` + fake `ApplyEvent`-stream. `Validate` (empty addr/command,
unknown state), `instance.pinged` happy-path (`Ping` → `Output.ok`, `changed=false`)
and error `Ping` → `failed`; `command.run` happy-path (`runCommand` → `ok`,
`changed` from params); **IS-invariant** - the password does not flow into events / in
sanitized connection error. Plus the **object boundary** (NIM-769): an object refuses
another object's action on the Apply path as well as on Validate, and a refused state
reaches no socket.
- **L0 user (localhost-exception)**
  ([`user_test.go`](../../../examples/module/soul-mod-mongo/user_test.go)):
  fake `mongoConn`. `Validate` (addr+name on both actions);
idempotency (present+is / absent+isn't → no-op); create/drop
(`changed=true`); **localhost-exception** (auth probe fails
`Unauthorized`/`AuthenticationFailed` → fallback to no-auth, `used_localhost`);
breeding `password` (connect) vs `user_password` (createUser-pwd);
**IS-invariant** (neither `password` nor `user_password` leaks).
- **L0 harness**
  ([`helpers_test.go`](../../../examples/module/soul-mod-mongo/helpers_test.go)):
general test inventory for L0 (fake `mongoConn`, fake `ApplyEvent`-stream,
`mustStruct`-params builder, assertion `assertEventsNoSecret` - check of information security invariant
"secret does not flow into events" used in `impl_test`/`user_test`).
- **L0 manifest ↔ implementation**
  ([`manifest_test.go`](../../../examples/module/soul-mod-mongo/manifest_test.go)):
the committed `schema.json` is what `mongoBundle` renders byte for byte, the object that
DECLARES a state is the one that SERVES it (both directions), every action declares
EXACTLY the params it reads, every secret param is `secret: true` + `^vault:.*`, and every
module declares `side: soul` (NIM-749). Plus, since NIM-800, that **every object carries
its declaration at runtime** — an object added later that forgot to wire it would silently
accept every coercion the type check exists to refuse.
- **L0 replicaset**
  ([`replicaset_test.go`](../../../examples/module/soul-mod-mongo/replicaset_test.go)):
  assertions are **on the wire** — which command was sent, to which node, carrying what
  document — because the invariants are not visible in a message. `replSetInitiate` is
  never sent at a set that has a config; a reconfig KEEPS `settings` and `protocolVersion`
  and every existing `_id`; a new member takes `max(_id)+1` and not an array index; the
  reconfig reaches the PRIMARY and not `params.addr`; a drifted attribute and an
  undeclared live member are REFUSALS and write nothing; NoReplicationEnabled (76) and
  NotYetInitialized (94) produce different messages; the localhost-exception bootstrap
  fires; a set with no primary within the budget FAILS. Plus the `Validate` member-rule
  table, including the direction that is easier to get wrong — `reconfigured` must NOT
  refuse a patch naming only `hidden`.
- **L0 params / type strictness**
  ([`params_test.go`](../../../examples/module/soul-mod-mongo/params_test.go)):
  `tls: "true"` as a string is REFUSED and **no connection is opened**. The assertion is
  on the refusal, deliberately — asserting instead that the password is absent from
  `argv` would pass on the broken build too, since the password was never in `argv`, it
  was in the connection.
- **L0 role / collection / index / database**
  (`role_test.go`, `collection_test.go`, `index_test.go`, `database_test.go`):
  create / no-op / update per object, and each no-op asserts that **no write command was
  sent**, not merely that `changed` was false. The two that carry the design: a live grant
  returned in a DIFFERENT order still reads as converged (`role`), and a compound index
  key declared in the reverse order is refused as a DIFFERENT index rather than reported
  as converged (`index`).
- **L0 shared fakes**
  ([`mongo_test.go`](../../../examples/module/soul-mod-mongo/mongo_test.go)):
  bson reply builders, a connection whose answers change across calls, and a module that
  hands out a different fake per address — the last one is what lets a test prove which
  node a reconfig reached.
- **L1** (integration, testcontainers mongo) - next batch.

`GOWORK=off go test ./...`. The `mongoConn` interface is unchanged by NIM-805 — every
command it added is a `runCommand` — so all of it is covered at L0 against a scripted
connection, with no live `mongod`.

## Assembly

```sh
cd examples/module/soul-mod-mongo
GOWORK=off go build ./...   # binary soul-mod-mongo (gitignored)
GOWORK=off go test ./...    # L0
```

Module - separate go.mod from `replace` to core (`../../../proto/plugin`,
`../../../sdk`); going standalone, not included in `go.work` (convention
`examples/module/`).

## See also

- [README.md](../README.md) - module directory (directory status).
- [redis](../redis/README.md) - the sibling plugin, re-laid-out onto the same address
  shape by NIM-766.
- [examples/service/mongo/](../../../examples/service/mongo/) - mongo service
  (PILOT, standalone): scenario `create` (install → render `mongod.conf` →
sysctl → systemd → start → bootstrap `default_admin` via localhost-exception
→ operator-users) and `destroy` (Soul-side teardown) call the states of this
plugin. Named type [`MongoUser`](../../../examples/service/mongo/types.yml)
(`types.yml`) describes the array element `input.users`.
- [ADR-012](../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, password
reaches the value.
- [ADR-031 Scry](../../adr/0031-scry-drift.md) — default-deny on dry_run without
  `PlanReadSafe`.
- [templating.md](../../templating.md) - secret masking (§7.4).
