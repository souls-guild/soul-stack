# dragonfly — per-host DragonFly building block (data plane)

Destiny `dragonfly` is a per-host building block for [DragonflyDB](https://www.dragonflydb.io/)
(the Ansible-role concept, B-hybrid [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
It idempotently installs DragonFly (distro-deb **or** upstream tarball), renders the
**flagfile** `dragonfly.conf` / `users.acl` / TLS-PEM, applies host-tuning extras
(THP/logrotate/sysctl), and brings up a **single-instance** service. The destiny itself
**does not know** the concrete install form — that's chosen by the service scenario via the
flags `deploy_dragonfly` / `install.method` and a ready merged config through `apply: input:`.

DragonFly is **wire-compatible with redis**, so the entire live runtime path (PING / REPLICAOF)
goes through the same `community.redis` plugin **with no changes**.

## Sentinel — NOT in this building block (sentinel comes from destiny `redis`)

DragonFly does not carry `redis-sentinel`, so the sentinel daemon over a DragonFly master
fully deploys the [`redis`](../redis/README.md) destiny in **sentinel_only** mode
(`deploy_redis: false` → the redis-server data plane is not deployed, only the
sentinel daemon over the external master goes up). The `dragonfly` service scenario makes a
**second** `apply: destiny: redis` alongside `apply: destiny: dragonfly` (two `apply:destiny`
in one scenario, [ADR-009](../../../docs/adr/0009-scenario-dsl.md)). That's why this building
block has **no** `sentinel.yml`, no sentinel templates, and no sentinel input — it's strictly
responsible for the DragonFly data plane.

## DragonFly vs `redis` (why a separate building block)

The structure copies the [`redis`](../redis/README.md) destiny with three things swapped:

- **config = flagfile** (`--flag=value`, absl flags), **not** `redis.conf`. Flag
  names use underscores (`tls_cert_file`, `snapshot_cron`), values without quotes
  (DragonFly explicitly rejects them). Bool flags are `--tls=true`. Template:
  [`dragonfly.flags.tmpl`](templates/dragonfly.flags.tmpl).
- **unit = `Type=simple`** (DragonFly runs foreground without `sd_notify`, unlike
  `Type=notify` for redis-server), `ExecStart … --flagfile`, hardening **inside**
  the unit (binary branch), not a drop-in. Template:
  [`dragonfly.service.tmpl`](templates/dragonfly.service.tmpl).
- **install = a single `dragonfly` binary** (tarball `dragonfly-<arch>.tar.gz`, arch
  `x86_64`/`aarch64`) **plus** a **separate** distro package `redis-tools` (`redis-cli` for
  health-gate / REPLICAOF) — DragonFly doesn't ship it.

**Out of scope** (PILOT): cluster (DragonFly cluster — emulated), redis modules `.so`,
version-guard for Redis 8+.

Logging uses **glog** (`--log_dir`, DragonFly names the files itself), not the redis-style
`logfile <path>`. Snapshots use `--snapshot_cron` / `--dbfilename` (redis's `save`/`appendonly`
don't apply to DragonFly).

Destiny version is a git ref ([ADR-007](../../../docs/adr/0007-versioning-git-ref.md)). Manifest
and task list — [`destiny.yml`](destiny.yml) and [`tasks/main.yml`](tasks/main.yml).

## Layout (tasks-split)

[`tasks/main.yml`](tasks/main.yml) is just an include list; groups are expanded inline into
a flat plan BEFORE render. Include order = task order.

| File | Tasks | Modules used |
|---|---|---|
| [`install.yml`](tasks/install.yml) | `redis-tools` (`redis-cli`, **unconditional**) + DragonFly — dispatched by `install.method`: **package** (distro-deb `dragonfly`) **or** **binary** (upstream tarball: fetch → extract → distro user/group → `core.file.present` (`src:`) laying out `dragonfly` into `/usr/local/bin` → its own systemd unit + ITS OWN restart) | `core.pkg`, `core.url`, `core.archive`, `core.group`, `core.user`, `core.file`, `core.service` |
| [`server.yml`](tasks/server.yml) | `dragonfly` data plane (gated by `deploy_dragonfly`): TLS-PEM (cert/key/ca) → `users.acl` → `dragonfly.conf` (flagfile) → `core.service running`. No hardening drop-in (hardening lives inside the unit itself) | `core.file`, `core.service` |
| [`extras.yml`](tasks/extras.yml) | host tuning, **unconditional**: disabling THP (oneshot unit) / logrotate / sysctl kernel parameters | `core.file`, `core.service`, `core.sysctl` |

## Reuse from `redis`

1:1 copies (DragonFly's ACL format is identical to redis's): `users.acl.tmpl`,
`disable-thp.service.tmpl`. `logrotate.tmpl` — DragonFly glog (sentinel daemon logs
are rotated by the `redis` destiny).
