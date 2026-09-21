# redis-exporter destiny

`redis-exporter` manages one named exporter instance per apply while sharing the
content-addressed `/usr/local/bin/redis_exporter` binary across the host. This lets Redis and
Sentinel expose isolated metrics endpoints on the same Soul without overwriting each other's
systemd or credential files.

## Instance contract

`instance_name` is required and accepts `^[a-z][a-z0-9-]{0,31}$`. For an instance named
`redis`, the destiny owns only:

- `redis_exporter-redis.service`;
- `/etc/default/redis_exporter-redis` (`0600`, `root:root`);
- `/etc/redis_exporter/redis/web.yml` (`0600`, `root:root`) when web TLS/basic-auth is enabled.

The Sentinel instance uses the same layout with `sentinel`. `ensure: absent` stops, disables,
and removes only the named instance. The common binary is removed only after no
`redis_exporter-*.service` unit remains, so removing Sentinel cannot interrupt Redis.

## Redis and Sentinel

```yaml
- name: Redis metrics
  apply:
    destiny: redis-exporter
    input:
      instance_name: redis
      sha256: "sha256:<release checksum>"
      listen: ":9121"
      redis_addr: unix:///var/run/redis/redis-server.sock
      dependency_unit: redis-server.service
      redis_user: monitoring
      redis_password: "${ vault('secret/redis/example/users/monitoring#password') }"

- name: Sentinel metrics
  apply:
    destiny: redis-exporter
    input:
      instance_name: sentinel
      sha256: "sha256:<release checksum>"
      listen: ":9122"
      redis_addr: redis://127.0.0.1:26379
      dependency_unit: redis-sentinel.service
      redis_user: monitoring
      redis_password: "${ vault('secret/redis/example/sentinel/monitoring#password') }"
```

`redis_addr` accepts `unix:///…`, `redis://…`, or `rediss://…`. The dependency is explicit
and independent of the transport, which is important for a local Sentinel reached over TCP.

## Secret boundary

Passwords and web-config hashes are secret inputs. Their render tasks write only root-owned
`0600` files; neither value appears in the systemd unit, `ExecStart`, incarnation state, or
task logs. A caller passing a declared secret ([ADR-0083] §1) hands over a `vault:` ref that
Keeper resolves at render — the plan and the audit trail carry the ref, not the value. Optional TLS key/cert and the web-config are imported with
systemd `LoadCredential`, so the `DynamicUser` process reads protected runtime copies rather
than loosening source-file permissions. `extra_args` is non-secret and must never carry a
password or token.

## Tests

Run the hermetic L0 suite with:

```text
soul-trial run examples/destiny/redis-exporter/_trial
```

The `multi-instance/two-apply-remove-one` case and its Go guard pin the invariant: two applies
render `:9121`/`:9122` units with different targets/dependencies, and Sentinel removal contains
no Redis teardown or unconditional shared-binary deletion.
