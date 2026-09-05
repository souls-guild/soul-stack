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

MAIN interface to **live MongoDB** (PILOT slice, role-based concept): service scenario orchestrates order/targeting/health-gate, and plugin
performs **one** operation on one `mongod` instance. Custom plugin
`kind: soul_module` serving **three objects** — `command`, `instance`, `user` — from one
binary, `soul-mod-mongo`; the artifact carries no name of its own (NIM-377), so level 1 is
whatever alias the operator registers it under. Implementation -
[`examples/module/soul-mod-mongo/`](../../../examples/module/soul-mod-mongo/)
(`object.go` - the object tables and the dispatch, `bundle.go` - the three `module.Def`s
the schema document is generated from, `obj_*.go` - one file per object's declaration,
`impl.go` - the shared driver + instance.pinged/command.run, `user.go` -
user.present/user.absent (createUser/dropUser + localhost-exception bootstrap),
`conn.go`/`tls.go` — connect, `helpers.go` - structpb/secret-helpers).

Backend — [`go.mongodb.org/mongo-driver`](https://go.mongodb.org/mongo-driver):
the plugin itself connects to `mongod` via TCP (`host:port`, usually `127.0.0.1:27017`).
Work is carried out **through the driver, and not `core.exec` + `mongosh`** (the password in `argv` is
IS risk, fragile output parsing; the same path `shell -> plugin` took
[`redis`](../redis/README.md)). Compliance with core modules
(`core.pkg`/`core.file`/`core.service`/`core.sysctl`) - for everything that is NOT
mongo-specific(installation, render `mongod.conf`, systemd, host-tuning); himself
MongoDB runtime is this plugin.

> **★ PILOT-scope (2026-06-30).** Implemented **four actions across three objects** for topology
> **standalone** (one `mongod`, `security.authorization: enabled`, admin via
> localhost-exception). OUTSIDE pilot (next slices, documented when
> will appear in the code): replica-set (`replSetInitiate`/add-member/member-synced),
> sharded (`mongos`/config/shard + [Choir](../../naming-rules.md)),
> `keyFile` (intra-cluster SCRAM authentication), TLS (mongo in pilot - plain;
> parameters `tls*` are declared in manifest for forward-compat, not in pilot script
> are specified).

## Without dry-run preview (consciously)

The plugin remains at `module.BaseModule` - **doesn't** implement `PlanReadSafe`
([ADR-031](../../adr/0031-scry-drift.md)) and `ErrandReadSafe`
([ADR-033](../../adr/0033-errand.md)). This is a conscious choice (parallel
`redis`): on `dry_run` host (Soul) applies **default-deny** - task
gets the honest "drift not supported" rather than the false "no drift".

## Objects and actions

Three objects (`schema.json::modules[]`), **four** actions between them. The object is
address level 2 — what is being managed — and the action is level 3: the state it is left
in, or, on the one non-stateful object, the single verb naming the operation.

| Object | Actions | What it manages |
|---|---|---|
| `command` | `run` | An arbitrary MongoDB command, run as given. Non-stateful, so level 3 is the verb form (`core.exec.run`'s shape). It takes exactly one verb — a second operation would be a second object. |
| `instance` | `pinged` | A live `mongod` as a whole: is it answering. |
| `user` | `present`, `absent` | One MongoDB user. Users live in `admin.system.users`, not in a config file (unlike the redis `users.acl` a destiny renders), so the subject is reached directly through `createUser`/`dropUser`. |

| Action | Destination | `changed` |
|---|---|---|
| `instance.pinged` | Health-probe via go-mongo-driver `Ping` (primary). Read-only. Replaces idiom `command.run { ping: 1 }` - health-gate in scripts (`retry`/`until`/`failed_when` by `register.self.ok`). | `false` **constructive** (probe, not change). |
| `user.present` | `createUser` (if the user is missing). Idempotent by `usersInfo`. ★ the first admin is created via **localhost-exception** (see below). | `true` on a real create; `false` (no-op) when the user already exists. |
| `user.absent` | `dropUser` (if the user is present). Idempotent by `usersInfo`. NO localhost-exception fallback — removal requires privileges. | `true` on a real drop; `false` (no-op) when the user is already gone. |
| `command.run` | Raw `db.runCommand` (imperative verb-action, precedent `redis.command.run`/`core.exec.run`). | `false` default (probe); `changed: true` in params - for actually mutating commands (the operator is responsible for idempotency). |

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
Idempotent by `usersInfo(name)`: `present` + user exists → no-op (change
password/roles of an existing user - day-2, outside pilot); `present` + no →
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

All four actions declare a **common** set of TLS connection parameters
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
  artifact-wide - all three objects work on a live `mongod`, and an operator approving
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
module declares `side: soul` (NIM-749).
- **L1** (integration, testcontainers mongo) - next batch.

`GOWORK=off go test ./...`.

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
