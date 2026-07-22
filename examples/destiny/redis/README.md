# redis — mode-agnostic per-host Redis brick

Destiny `redis` is a **single** per-host Redis brick for all deployment modes
(B-hybrid [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
It idempotently installs `redis-server`, renders `redis.conf` / `users.acl` / TLS-PEM /
the systemd-hardening drop-in, applies host-tuning extras (THP/logrotate/sysctl), and
brings up **a single instance** of the service. The destiny itself **does not know**
the specific shape of the instance — that is chosen by the service scenario via the
`deploy_redis` / `sentinel_enabled` / `install.method` flags and a ready-made merged
config passed through `apply: input:`. Combinations of flags cover both cluster and
sentinel (master-replica + sentinel daemon), and a **thin sentinel layer over an
external master** (`deploy_redis: false`) — the latter remains a **brick capability**
for reuse by other services (e.g. DragonFly), even though the [`redis`](../../service/redis/README.md)
service deploys only `redis_type ∈ [sentinel, cluster]` modes (`standalone`/`sentinel_only`
were removed from the service on 2026-06-25).

The brick stays **"dumb"**: all orchestration — topology, master election,
rolling restart, failover, sentinel reconcile, health-gate, merging `redis.conf` —
lives in the service scenario [`examples/service/redis/`](../../service/redis/README.md).
The destiny receives values already resolved (config as a ready-made merged map,
secrets via `vault()` in Keeper's render phase) and only materializes them on the host.

The destiny version is a git ref ([ADR-007](../../../docs/adr/0007-versioning-git-ref.md));
there is no top-level `version:` field. The manifest and task list are
[`destiny.yml`](destiny.yml) and [`tasks/main.yml`](tasks/main.yml).

## Composition (tasks-split)

[`tasks/main.yml`](tasks/main.yml) is include-list only; logical groups are split
across neighboring files and expanded inline into a flat plan BEFORE render ([tasks.md §4](../../../docs/destiny/tasks.md)).
Include order = task order (the final flat-plan indices depend on it, and L0 tests
assert against them):

| File | Tasks | Modules used |
|---|---|---|
| [`install.yml`](tasks/install.yml) | installing redis binaries — dispatched by `install.method`: **package** (distro default package) **or** **binary** (default — separate binaries fetched from Nexus: distro user/group → **four** `core.url.fetched` fetch `redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` into `/usr/local/bin` (URL `<base_url>/<arch>/Debian/<distro_ver>/<version>/…`) → symlinks `redis-check-aof`/`redis-check-rdb` → its own systemd unit + its OWN restart) + unix-socket directory (both branches) | [`core.pkg`](../../../docs/module/core/pkg/README.md), [`core.url`](../../../docs/module/core/url/README.md), [`core.cmd`](../../../docs/module/core/cmd/README.md), [`core.group`](../../../docs/module/core/group/README.md), [`core.user`](../../../docs/module/core/user/README.md), [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`server.yml`](tasks/server.yml) | data plane `redis-server` (gated by `deploy_redis`): TLS-PEM (cert/key/ca) → `users.acl` → `redis.conf` → systemd-hardening drop-in → `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`sentinel.yml`](tasks/sentinel.yml) | sentinel daemon (gated by `sentinel_enabled`): `sentinel-users.acl` (2nd aclfile) → `sentinel.conf` → systemd unit → `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`extras.yml`](tasks/extras.yml) | host-tuning, **unconditional** (a Redis recommendation / hardening, not an operator choice): disabling THP (oneshot unit) / logrotate / sysctl kernel parameters | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md), [`core.sysctl`](../../../docs/module/core/sysctl/README.md) |
| [`modules.yml`](tasks/modules.yml) | `.so` directory + fetching Redis modules (RediSearch/RedisJSON/RedisTimeSeries/RedisBloom). The whole file is gated (`vars.redis_modules_enabled`): enabled when the data plane is on **AND** Redis < 8 **AND** `modules_base_url` is NON-EMPTY; otherwise a group-drop — an empty `modules_base_url` gives **vanilla** redis (no `loadmodule`/fetch) | [`core.file`](../../../docs/module/core/file/README.md), [`core.url`](../../../docs/module/core/url/README.md) |

All core modules — no `required_modules:` at all ([`community.redis`](../../../docs/module/community/redis/README.md)
is called from the service scenario, not from this destiny).

Mode gates are implemented via static-skip ([ADR-009](../../../docs/adr/0009-scenario-dsl.md)
block-static-skip): predicates `deploy_redis` / `sentinel_enabled` / `install.method`
depend only on `input.*`, so Keeper mutes inactive branches with a placeholder **without
shifting indices** (a skip still occupies a slot; the flat-register-scope is invariant).
Register references in `onchanges` (cert rotation → redis restart, etc.) **must live in
the same file** as their consumer: the load-time per-file linter rejects cross-file
`onchanges`. That's why the binary branch carries its own `redis-server` restart right
inside [`install.yml`](tasks/install.yml).

## Input contract (overview)

The destiny sees **only its own** `input:` (isolation, [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
The full typed schema with a description of every field is [`destiny.yml → input:`](destiny.yml);
how the service scenario builds and passes these values to the operator is in
[service-README → Input contract](../../service/redis/README.md). Key groups:

- **Data-plane gate.** `deploy_redis` (bool, default `true`) — whether to deploy
  `redis-server`. `false` (the `sentinel_only` mode) mutes the whole data plane
  ([`server.yml`](tasks/server.yml)); the redis package is still installed
  regardless (it also carries the sentinel daemon).
- **`install`** — how binaries are delivered: `{method, base_url, version}`.
  `method=package` — a distro package; `method=binary` — **separate binaries fetched
  from Nexus** (`redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` are fetched
  per-host from `<base_url>/<arch>/Debian/<distro_ver>/<version>/…` by content-idempotency
  SHA-256, without integrity-verify — `redis-sentinel` is a separate binary,
  `redis-check-aof`/`redis-check-rdb` are symlinks to `redis-server`). The top-level
  `version` (distro pin) is for the package branch; `install.version` is for the binary
  branch. **The service-level `install_method` (default `binary`) sets the installation
  method, while `base_url`/`version` of the binary come from `essence`** (see
  [service-README](../../service/redis/README.md)); the destiny assembles the `install`
  struct from them.
- **TLS.** `tls: {enable, only, port, cert_ref, key_ref, ca_ref}` — a single dict
  (host-invariant). PEM material is rendered **through `core.file.present` +
  `${ vault(ref) }` in the `content` cell** (seal-masking, [templating.md §7.4](../../../docs/templating.md)),
  **not** via `.tmpl` and **not** as an already-resolved PEM through `apply.input`: the
  destiny receives Vault **paths** (`cert_ref`/`key_ref`/`ca_ref`) and reads the value
  itself during Keeper's render phase; no Vault client is pulled into Soul
  ([ADR-012](../../../docs/adr/0012-keeper-soul-grpc.md)).
- **Secrets.** `password` (`requirepass`) and `users` (an ACL map `name → {perms,
  state, password}` for redis-server) arrive already resolved keeper-side; `users.acl`
  is written with a sha256 hash of the password, plaintext is never materialized.
  They are masked in logs/traces/UI. The `users` set is assembled by the service
  scenario as **system + operator-extra** (see
  ["System ACL users and the second aclfile"](#system-acl-users-and-the-second-aclfile)).
- **`sentinel_users`** (optional, additive) — an ACL map of the same shape for the
  **sentinel daemon**, rendered into a **separate** `sentinel-users.acl` (see below).
  Passed only in sentinel mode; empty/absent → empty aclfile (back-compat).
- **Config.** `config` — a **ready-made merged map** of `redis.conf` directives
  (default → preset → computations → passthrough is done by the service scenario).
  The service scenario also places persistence/`maxmemory`/`maxmemory-policy`/
  `unixsocket`/TLS- and cluster-directives here. The destiny does **not** re-merge them.
- **Host-tuning.** `sysctl_settings` (map kernel-parameter → value, strings) → a
  `/etc/sysctl.d/30-redis.conf` drop-in via [`core.sysctl.applied`](../../../docs/module/core/sysctl/README.md).
- **Redis modules.** `modules_base_url` — the only modules-related field (`modules`/
  `modules_dir` fields were removed: the set is a brick invariant `vars.redis_modules`,
  `modules_dir` is derived from `data_dir`). The "modules are **always all**"
  directive is all-or-nothing: a non-empty `modules_base_url` on Redis < 8 fetches the
  **whole** set (RediSearch/RedisJSON/RedisTimeSeries/RedisBloom); an **empty**
  `modules_base_url` → **vanilla** redis (the `modules.yml` group group-drops, not a
  single `.so`). `.so` files are fetched by content-idempotency (SHA-256), the URL is
  arch-specific (`soulprint.self.os.arch` per host).
- **Sentinel.** `sentinel_enabled` (bool) + `sentinel: {master_name, master_ip,
  master_port, quorum, auth_user, auth_pass, config}` — used only in sentinel modes;
  `master_ip` is required whenever the dict is passed.

## System ACL users and the second aclfile

The brick renders **two** ACL files, both `core.file.rendered`, both `mode 0640`
owner/group `redis`, both writing the password as a **hash** (`#<sha256>`, sprig
`sha256sum`), never plaintext:

| File | Template | Source of `vars.users` | aclfile directive |
|---|---|---|---|
| `${conf_dir}/users.acl` | [`users.acl.tmpl`](templates/users.acl.tmpl) | `input.users` (system redis-server + operator-extra) | `aclfile` in `redis.conf` |
| `${conf_dir}/sentinel-users.acl` | [`sentinel-users.acl.tmpl`](templates/sentinel-users.acl.tmpl) | `input.sentinel_users` (system sentinel-daemon) | `aclfile` in `sentinel.conf` |

**Two files, because redis-server and the sentinel daemon have different perms** —
the sentinel daemon needs `sentinel|*` commands rather than ordinary redis commands,
so their system user sets don't overlap. Which set is placed (`replica`/`monitoring`/`sentinel`/
`haproxy`, plus `default` only in the sentinel-aclfile) and where the passwords come
from is decided by the **service scenario**; the destiny receives the ready-made map
and only renders it. See
[service-README → "System ACL users"](../../service/redis/README.md#system-acl-users).

`sentinel-users.acl` is rendered as the **first** task of [`sentinel.yml`](tasks/sentinel.yml)
(BEFORE `sentinel.conf` — its `aclfile` directive must point to an already-existing
file), under `register: redis_sentinel_acl`. The sentinel daemon's ACL is reread only
via a **restart** of the daemon (`onchanges: [redis_sentinel_conf, redis_sentinel_acl,
redis_sentinel_unit]`) — there is no separate hot-reload path for the sentinel-aclfile:
the set is a system one, changed only on create/rotation. An empty map → a valid empty
aclfile (back-compat: a destiny at an old ref without the `sentinel_users` field
renders an empty file, the directive is harmless).

> **The `default` user: absent from `users.acl`, present in `sentinel-users.acl`.**
> redis-server authorizes `default` via `requirepass` in `redis.conf` (`users.acl`
> does not duplicate it). The sentinel daemon has no `requirepass` equivalent for
> aclfile access, so `default` is declared directly in `sentinel-users.acl`.

## What this is NOT

This is a **destiny brick** (a per-host task package for a single instance), **not a
service**. There is no `state_schema`, no migrations, no simple-input operator
(`memory_mb` / `persistence` / `shards` / `redis_type`), no translation into
`redis_config`, no orchestration and no operational scenarios — all of that lives in
the service wrapper [`examples/service/redis/`](../../service/redis/README.md).
The destiny does not decide which mode is deployed and does not call the
[`community.redis`](../../../docs/module/community/redis/README.md) plugin (a live
Redis is the service scenario's territory).

## References

- [`examples/service/redis/`](../../service/redis/README.md) — service wrapper:
  operator input contract, `state_schema`, translation, orchestration, modes, and
  operational scenarios.
- [`destiny.yml`](destiny.yml) — manifest and the full `input:` schema.
- [docs/destiny/](../../../docs/destiny/README.md) — destiny format and `tasks:`/`include:` mechanics.
- [docs/templating.md](../../../docs/templating.md) — CEL + text/template, `${ vault(ref) }`, seal-masking.
- Per-module README for the modules used — see the "modules" column in the composition table above.
