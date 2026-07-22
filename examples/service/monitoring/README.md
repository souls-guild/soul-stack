# monitoring

An example service that deploys Prometheus exporters on VMs:

- **node-exporter** — system metrics, listens on the standard `:9100` by default.
  Delegated to the reusable standalone destiny
  [`node-exporter`](../../destiny/node-exporter/) via
  `apply:destiny` (input isolation, [ADR-009](../../../docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)):
  the `node_exporter` binary under a **stable system account `node_exporter`**
  (stateful branch), version-aware install + hardened unit, `arch` from soulprint.
  Optional hardware textfile collectors (smartmon/nvme/ipmi) are disabled here
  (`node_exporter_collectors: []`) — there's no hardware on a VM, only the core is installed;
- **redis_exporter** — Redis metrics, connects to Redis **via a unix socket**
  (`--redis.addr=unix:///<path>`), listens on `:9121` by default. Runs under
  a dedicated system user `redis-exporter` in the `redis` group
  (least-privilege access to the socket). **Stays INLINE** (not `apply:destiny`)
  deliberately — see "Assumptions" below.

Authored on existing core MVP modules ([ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)),
without new core Go code.

> **Why redis_exporter is inline while node-exporter is a destiny.** The prod destiny
> [`redis-exporter`](../../destiny/redis-exporter/) requires
> `redis_password` (required) and carries `Requires=redis-server.service` in its unit —
> it's designed for a host with a local Redis behind a password. The purpose of the
> `monitoring` service is different: exporters on a host with an **already running** Redis
> (possibly without `requirepass`); the service does NOT install Redis itself. Reuse would
> break this and would require extending the public contract with a new required field. The
> drift with node-exporter has been removed; redis_exporter here is a separate case, not a
> duplicate of the canonical one.

## Assumptions

- **Target:** Linux + systemd (Debian/Ubuntu/RHEL/Alpine with systemd).
- **redis_exporter ↔ Redis — via a unix socket**, the socket path is the
  `redis_socket` parameter (default `/var/run/redis/redis-server.sock`).
- **The `redis` group already exists on the host.** This service does NOT install Redis — it
  connects to an already running Redis. The socket is created by redis-server with
  `unixsocketperm 770` owner `redis:redis`, so redis_exporter runs under
  a dedicated system user `redis-exporter` (`core.user` step, no-login),
  included in the `redis` supplementary group — access to the socket via "group" bits.
  The `redis` group is created by the redis-server package; if Redis is on the host, the
  group is already there. `DynamicUser` for redis_exporter is **not used**: an ephemeral
  systemd UID is not a member of the `redis` group and would get `EACCES` on `connect()`.
  node-exporter doesn't require a socket; its account is a stable system user
  `node_exporter` (stateful branch: the textfile directory `/var/lib/node_exporter`
  survives restarts and is read by root collectors), this is handled by the destiny
  `node-exporter` itself. If the `redis`
  group is missing (Redis not yet installed on the host) — the `core.user` step will fail; in
  this case install Redis beforehand or add `core.group.present name: redis`.
- **Exporter versions** are pinned via input to specific releases
  (`node_exporter_version`, `redis_exporter_version`). For redis_exporter
  (inline fetch) the operator must supply the tarball checksum along with the version
  (`redis_exporter_sha256`, `required: true`); node_exporter is downloaded via the
  `node-exporter` destiny, whose checksum input was removed (download without verification).
- **Install — a single "triple" pattern:** `core.url.fetched` (download the tarball from
  GitHub Releases) → `core.archive.extracted` (unpack) → `core.cmd.shell`
  (`install -m0755` the binary into `bin_dir`). The download is done by the specialized
  `core.url.fetched` — **https-only**; when a checksum is given — **verification BEFORE
  publishing the file** (a wrong hash never gets materialized, supply-chain), when the
  checksum is empty — idempotency by content SHA-256. `core.cmd.shell` remains only
  for the local install step (`core.file.present` cannot copy from a path —
  only inline content), it has no network access.
- **Checksum for redis_exporter is a required input** (`redis_exporter_sha256`,
  form `"sha256:<hex>"`, `required: true` **with no default**, fail-closed per
  [prod convention §7](../../../docs/destiny/production-conventions.md#7-supply-chain)).
  No default: no hash → an honest resolve failure, not a fetch with a placeholder.
  The hash comes from `sha256sums.txt` of the relevant GitHub release for the pair
  (version, arch); `core.url.fetched` verifies it BEFORE publishing the file.

If any of these assumptions don't hold (e.g. you need a package from the distro repository
instead of a tarball, or a TCP connection to Redis instead of a socket) — adjust the input/scenario.

## Layout

```
monitoring/
├── service.yml                              # manifest: state_schema_version=1,
│                                            #   destiny[] (node-exporter),
│                                            #   state_schema {node/redis versions, redis_socket}
├── essence/
│   └── _default.yaml                        # baseline: versions + socket path (substrate)
└── scenario/
    └── create/
        ├── main.yml                         # input + tasks (apply:destiny node-exporter
        │                                     #   + inline redis_exporter) + state_changes
        ├── templates/
        │   └── redis_exporter.service.tmpl   # redis_exporter systemd unit (inline, unix socket)
        └── tests/
            ├── render-defaults/case.yml     # L0-trial (render-only, defaults)
            └── allow-private-override/case.yml  # L0-trial: SSRF-guard opt-out (allow_private: true)
```

`node_exporter.service.tmpl` was removed: node-exporter is rendered by its own destiny.
`redis_exporter.service.tmpl` remains — used by the inline redis_exporter block.

There is no `migrations/` directory: `state_schema_version = 1` ([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## What the `create` scenario does

On each host of the incarnation:

1. **node-exporter** — a single `apply:destiny` into
   [`node-exporter`](../../destiny/node-exporter/): the
   `core.url.fetched` (checksum-pinned) → `core.archive.extracted` → version-aware
   `core.cmd.shell install` (the binary is laid out as `node_exporter`,
   `unless --version` catches upgrades) → `core.group.present`/`core.user.present`
   (stable system account `node_exporter`) → `core.file.rendered` hardened unit
   → `core.service.running` (`enabled: true`) + restart on `onchanges` triple. With
   `node_exporter_collectors: []` (service default), only the core is installed; the
   privileged-collector steps are disabled via `when:`. The scenario
   forwards its input to the destiny; `arch` is taken from `soulprint.self.os.arch`.
2. **redis_exporter** — inline (not `apply:destiny`, see above): the same triple
   url.fetched → archive.extracted → install → `core.user.present`
   (`redis-exporter` in the `redis` group) → `core.file.rendered` unit (no password,
   `User=redis-exporter`) → `core.service.enabled` → `core.service.running` →
   restart on `onchanges`. The tarball name carries a `v` prefix before the version
   (`redis_exporter-v<v>.linux-<arch>`).

`core.service.enabled` and `core.service.running` in the inline redis_exporter block are
**two separate steps**: the `running` state of the `core.service` module only manages
activity and does not read the `enabled` parameter. (The node-exporter destiny uses a
single enable idiom — `enabled: true` inside `running`.)

### Connecting redis_exporter via a unix socket

In the scenario, `redis_addr` is assembled by CEL: `${ 'unix://' + input.redis_socket }`
and forwarded into `redis_exporter.service.tmpl`, where it lands in
`ExecStart … --redis.addr={{ .vars.redis_addr }}`. With the default socket, the result is
`--redis.addr=unix:///var/run/redis/redis-server.sock`.

## Input contract (docs/input.md)

| Parameter | Type | Default | Purpose |
|---|---|---|---|
| `node_exporter_version` | string (semver-like) | `1.8.2` | node_exporter version. |
| `redis_exporter_version` | string (semver-like) | `1.62.0` | redis_exporter version. |
| `redis_exporter_sha256` | string `sha256:<hex>`, **required** | — | SHA-256 of the redis_exporter tarball. From the `sha256sums` of the GitHub release for the pair (version, arch). |
| `arch` | enum `amd64`/`arm64` | `amd64` | Architecture of the release tarball. Set in the scenario contract, but both destinies take arch directly from `soulprint.self.os.arch` — the input never reaches the tasks (one incarnation can mix amd64/arm64 hosts). |
| `bin_dir` | string (abs path) | `/usr/local/bin` | Where binaries are laid out. |
| `redis_socket` | string (abs path) | `/var/run/redis/redis-server.sock` | Redis unix socket. |
| `node_exporter_listen` | string `host:port` | `:9100` | node_exporter listen address (`--web.listen-address`). |
| `node_exporter_collectors` | array enum `smartmon`/`nvme`/`ipmi` | `[]` | Which hardware textfile collectors node-exporter installs. `[]` = core only (no hardware on a VM). |
| `node_exporter_allow_private` | boolean | `false` | SSRF-guard opt-out for the `node-exporter` destiny: allow tarball fetch when `base_url` resolves to a private address (internal mirror). Forwarded into `core.url.fetched` (`allow_private`). Default `base_url` is public GitHub, the guard stays on. |
| `redis_exporter_listen` | string `host:port` | `:9121` | redis_exporter listen address. |
| `redis_exporter_extra_args` | array string | `[]` | Extra flags for the inline redis_exporter (`--check-keys=…`, `--web.config.file=…` for TLS/basic-auth, etc.). Each element is a separate `ExecStart` token. |

The checksum parameter for redis_exporter is mandatory (`redis_exporter_sha256`, fail-closed per
[prod convention §7](../../../docs/destiny/production-conventions.md#7-supply-chain));
node_exporter is downloaded via the `node-exporter` destiny without a checksum; the rest are defaults.

## Idempotency

All steps are re-applicable:

- `core.url.fetched` — checksum matches the already-downloaded tarball → no-op (content
  isn't re-downloaded);
- `core.archive.extracted` — the `.soul-archive.sha256` marker file matches → no-op
  (doesn't re-extract the same archive);
- `core.cmd.shell` — `creates: <bin>` → no-op if the binary is already installed;
- `core.file.rendered` — writes the file only on a content diff (SHA-256);
- `core.user.present` — no-op if the `redis-exporter` user already exists
  (present-or-create, no group reconcile in MVP — see `coremod/user`);
- `core.service.enabled` / `core.service.running` — declarative, no-op if already
  enabled/active;
- `core.service.restarted` — fires only on `onchanges` from a changed unit.

## Validation

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/monitoring/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/monitoring/scenario/create/main.yml
```

Both give exit 0 and `OK: <path>`.

## L0-trial (render-only)

```bash
./keeper/bin/soul-trial run examples/service/monitoring/scenario/create/tests/render-defaults/case.yml
```

The case is hermetic (render-only, downloads nothing): checks the CEL render of tasks with
standard inputs. The plan is the node-exporter destiny composite (`apply:destiny`, indices
0..5) + the inline redis_exporter (6..13). Asserts the url assembly from `version`+`arch`
(`arch` from soulprint) for `core.url.fetched` (checksum-pin only for redis_exporter),
assembly of `redis_addr=unix://...`, creation of the `redis-exporter` user
(`core.user.present`), and the declarative `core.service.{enabled,running}` steps. Gives
`PASS`.

The second case — `tests/allow-private-override/case.yml`: checks that
`node_exporter_allow_private: true` reaches, via `apply: input:`, the
`core.url.fetched` of the destiny (`allow_private: true`, SSRF guard lifted);
in render-defaults the guard stays on (`allow_private: false`).

> `apply:destiny node-exporter` resolves the prod mirror (slice A, ADR-023): name →
> `service.yml::destiny[]` + URL from the case's `fixtures.default_destiny_source`
> (`file://../../destiny/{name}`, hermetic). `fixtures.soulprint.os.arch`
> gives the host's `arch`, which the exporters take from `soulprint.self.os.arch`.

> **Real run** (installing binaries, bringing up services) requires Linux+systemd
> and network access to GitHub Releases — not run on a dev Mac. The L0-trial
> covers the render phase; an integration run is done on a Linux rig via
> `keeper.push` / the pull agent. `redis_exporter_sha256` is a mandatory parameter
> (no default): for a real run, supply a real hash from the `sha256sums`
> of the GitHub release, otherwise `core.url.fetched` will fail on checksum mismatch (and
> without a value at all — the resolve fails fail-closed). node_exporter is downloaded via the
> `node-exporter` destiny with no checksum (idempotency by content SHA-256).

## Deliberately not present here

- `migrations/` — `state_schema_version = 1`.
- `node_exporter.service.tmpl` — no: node-exporter is rendered by its own destiny
  (`redis_exporter.service.tmpl` remains for the inline block).
- `on:` / `where:` — an omitted `on:` means "the entire incarnation"
  ([orchestration.md §3](../../../docs/scenario/orchestration.md)).
- `core.cmd.shell` for downloading — downloads are done by `core.url.fetched`
  (https-only + checksum); `core.cmd.shell` is kept only for the local
  install step (see "Assumptions").
