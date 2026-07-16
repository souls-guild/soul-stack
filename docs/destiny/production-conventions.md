# Food Convention destiny

Checklist "product-grade destiny" - what distinguishes the reference production-ready destiny from the draft one. These are **normative** rules for destiny, which install and maintain a system service on the host (daemons under systemd: exporters, redis, postgres, etc.). Reference source - [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/); other service-destinies are reduced to the same pattern.

**There is only one standard - `node-exporter` (stateful branch).** In `examples/destiny/node-exporter/` there is a generic-destiny Prometheus node_exporter: the binary, user, group and unit are called `node_exporter`, the daemon works under a **manual stable system account `node_exporter`** (stateful branch §2 — the textfile directory of hardware metrics survives restarts and is read by root collectors, a stable uid owner is needed), carries systemd-hardening (§3), **version-aware install** (`unless --version` → upgrading the binary will restart the service, §6), optional `checksum` for mirrors (§7) and **privileged textfile collectors** smartmon / nvme / ipmi with separate oneshot timers under a hard systemd-sandbox (§3b). The name "node-exporter" in `apply: { destiny: node-exporter }` below refers specifically to this standard.

> **DynamicUser remains a valid choice** for **stateless** daemons (§2, stateless branch) - it's just that the current standard does not illustrate it, because node-exporter stateful (textfile directory, see §2). The rule for selecting an account (DynamicUser vs manual uid) is normative and does not depend on which example shows it.

They rely on [ADR-009](../adr/0009-scenario-dsl.md) (destiny isolation), [ADR-015](../adr/0015-core-modules-mvp.md) (a set of core modules) and the ["safety first"](../requirements.md) invariant from requirements.md.

## 1. Passthrough flags and config

Don't put a closed list of demon flags into destiny. Declare `extra_args` (`type: array`, `items: { type: string }`, `default: []`) as an end-to-end channel for any flags beyond the basic ones.

- Passed to the template as a native list: the entire cell `vars` = one `${ input.extra_args }` → according to rule ADR-010 "non-string CEL result, entire cell = `${…}`" a real list is received, not a string.
- In `.tmpl` the `range` is expanded, each element is a separate `ExecStart` token, not separated by spaces:

  ```gotemplate
  ExecStart={{ .vars.bin_dir }}/<daemon> --web.listen-address={{ .vars.listen }}{{ range .vars.extra_args }} {{ . }}{{ end }}
  ```

- The value-with-space is passed as a single list element: `["--web.config.file=/etc/x.yml"]`, not `["--web.config.file", "/etc/x.yml"]` via template concatenation.

This allows the operator to expand the behavior of the daemon (collectors, textfile-dir, TLS config) without editing destiny itself. Working illustration - `redis_exporter_extra_args` in the inline block `redis_exporter` of the script [`monitoring/scenario/create`](../../examples/service/monitoring/scenario/create/main.yml) (goes to `redis_exporter.service.tmpl` through `range`). The reference `node-exporter` itself does not declare `extra_args` - it instead puts specific settings into named `input` parameters (`listen`/`collectors`/`bin_dir`/`user`/`textfile_dir`/...); both approaches are valid, the choice is between "an open channel of any flags" and "an explicit typed contract".

## 2. Service account - hybrid rule

**Regulatory rule** how a service gets an unprivileged account. The choice is determined by whether the daemon owns stable state on the disk:

| Service type | Sign | Account |
|---|---|---|
| **Stateless-demon** | no owned data-dir, does not write anything under a fixed uid (exporters, proxies without local state) | `DynamicUser=yes` in unit. systemd itself creates a locked transient user for the lifetime of the process. Manual `core.user`/`core.group` **WILL NOT start.** |
| **Stateful-service** | owns the data directory, needs a stable uid for file rights between restarts and upgrades (redis, postgres) | Manual system-uid via `core.user.present`/`core.group.present` (no-login shell, no home, no gid/uid - system range). It is referenced by `User=`/`Group=` in unit. |

Why this way:

- For stateless `DynamicUser=yes` is strictly safer than a manual account - the uid is ephemeral, it is not reused between restarts, the user does not hang in `/etc/passwd`, there is nothing to capture. A manual account here is an extra surface with no benefit.
- For stateful `DynamicUser` is unsuitable: data files created under the transient-uid of one run belong to a "foreign" uid on the next run. Here you need a stable account.

Do not mix: stateless-destiny with `DynamicUser` should not have a manual user "just in case", stateful should not rely on `DynamicUser` for data-dir.

Illustrations from both threads:

- **Stateful + manual system account** - reference [`node-exporter/`](../../examples/destiny/node-exporter/): `core.group.present` + `core.user.present` (`system: true`, `shell: /usr/sbin/nologin`, `home: /`) starts `node_exporter`, which is referenced `User=`/`Group=` in unit. A stable uid is needed because... the textfile directory `--collector.textfile.directory` (`/var/lib/node_exporter`) is owned by this account and survives restarts, and privileged oneshot collectors write to it (§3b); the group is created **before** the user (`core.user -g` requires an existing primary group). This is not an "account from a distribution package" (§3a, redis), but an explicitly created destiny system account - a valid stateful path when the service is installed not by a package, but from a tarball release.
- **Stateless + `DynamicUser`** - there is no separate destiny example in the repository now, but the branch remains normative: a stateless daemon without owned data-dir (exporter without textfile directory, proxy without local state) receives an account from systemd itself through `DynamicUser=yes` in the unit, manual `core.user`/`core.group` will not start.

## 3. systemd-hardening - required in all unit templates

Every rendered systemd-unit carries a hardening block. This is a direct consequence of the "safety first" invariant ([requirements.md](../requirements.md)); at the time of the introduction of the convention, not a single example had it - it is a closed gap, not "optional".

Basic block (static template text, not included in `render_context`):

```ini
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
CapabilityBoundingSet=
```

Rules for setting up a specific daemon:

- `RestrictAddressFamilies` - add `AF_UNIX` only if the daemon actually listens/uses the unix socket. A pure TCP daemon (node_exporter) makes do with `AF_INET AF_INET6`.
- `CapabilityBoundingSet=` (empty) - for daemons that do not need capabilities (exporters read public kernel/FS metrics). If the daemon really needs a capability (for example, `CAP_NET_BIND_SERVICE` for port <1024), list it explicitly, do not open the entire set.
- `ProtectSystem=strict` makes the entire `/` read-only. If the daemon only reads (`/proc`, `/sys`) that is enough, `ReadWritePaths` is not needed. Add `ReadWritePaths=<dir>` **only** under the actual recording directory (data-dir of the stateful service), and only that - do not expand "just in case".

### 3a. Stateful option: ReadWritePaths + drop-in (redis)

The basic block above is for a daemon with **its own renderable unit** (node-exporter is installed from the tarball release and renders the entire unit). The Stateful service, which is installed by the **distribution package** (redis, postgres), differs in two things:

- **Hardening arrives by drop-in, and not by its own unit.** The distribution unit cannot be replaced entirely - it carries `ExecStart`/`Type=notify`/`RuntimeDirectory` and is updated along with the package. Override is placed as a separate task `core.file.rendered` in `/etc/systemd/system/<unit>.service.d/hardening.conf` (`mode 0644`, `owner/group root` is a systemd file, not a service account). The `[Service]` directives are merged on top of the distribution ones, ours take precedence.
- **`ProtectSystem=strict` is required to carry `ReadWritePaths`** under all directories where the service writes, otherwise strict will make them read-only and break writing - this is a **prod-incident**, not "stricter". The paths are taken from the service config (for redis - `dir`/`unixsocket`/`pidfile`/`logfile` to `redis.conf`); `ReadWritePaths` lists their **directories**, and only them.

Drop-in example for redis (`redis-server.service.d/hardening.conf`):

```ini
[Service]
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/redis /var/run/redis /var/log/redis
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
RestrictNamespaces=yes
CapabilityBoundingSet=
```

Differences from the stateless block and their reasons:

- **`User=`/`Group=` are NOT specified in the drop-in.** Account - from the package: the distribution package creates a system account (`redis:redis`) with a stable uid, and the distribution unit is already launched under it. This is the stateful path §2 (stable uid-owner of the data-dir between restarts/upgrades), `DynamicUser` is unsuitable here. Manual `core.user`/`core.group` are not needed for the account from the package.
- **`MemoryDenyWriteExecute` is NOT included** (unlike Go exporter binaries, where it is appropriate). redis - in C, uses jemalloc and supports loadable modules; MDWE prohibits W+X mappings and is capable of breaking module allocator/JIT pages. For a C-service with such a profile, do not add MDWE.
- **`RestrictAddressFamilies` carries `AF_UNIX`** - redis listens to BOTH TCP AND a unix socket (add the list to the actual service families, §3 rule).

**daemon-reload + restart to change drop-in.** Drop-in changes an already loaded unit, so `core.service.restarted` alone is not enough - systemd must first re-read the configuration.

> **From the version with centralized daemon-reload in `core.service` ([ADR-015 Amendment 2026-06-18](../adr/0015-core-modules-mvp.md)), the separate step `core.exec.run systemctl daemon-reload` for restart has become OPTIONAL.** `core.service.restarted`/`running`/`enabled` by default (`daemon_reload: auto`) themselves check the systemd flag `NeedDaemonReload` and re-read unit before action - the changed drop-in has already been taken into account. **It is preferable to rely on the built-in `auto`** and not duplicate reload as a separate task. See [docs/module/core/service/README.md → daemon_reload](../module/core/service/README.md#daemon_reload---rereading-unit-files).

The manual pattern below remains valid (for example, when reload is needed explicitly outside of the service restart, or for historical destiny). Reactive and idempotent chaining:

1. `core.file.rendered` drop-in (`register: <hardening>`).
2. `core.exec.run systemctl daemon-reload` with `onchanges: [<hardening>]` - reread unit before restart.
3. `core.service.restarted` with `onchanges: [<hardening>]` - restart when the drop-in is changed (the changed drop-in takes effect only after the restart).

The order is guaranteed by the position of the tasks (drop-in → daemon-reload → restart). Nothing has changed → the whole chain is no-op. Based on the built-in `auto`, step 2 is omitted: `core.service.restarted` (`daemon_reload: auto`) does the reload itself before restarting.

> **★ Restart is narrowed to the unit level (§6a).** In `onchanges` restart - **only** register drop-in tasks (`<hardening>`), **not** config/ACL/TLS material registers. For a service with live-reconfig (Redis), changes to `redis.conf`/`users.acl`/TLS PEM **do not restart** the process: they are revived by the hot-reload day-2 script (`update_config`/`add_user`/`rotate_tls`, §6a). A restart remains a true case only for changing the systemd unit itself (hardening drop-in), which CONFIG SET cannot cover. Reference - [`examples/destiny/redis/tasks/server.yml`](../../examples/destiny/redis/tasks/server.yml): task `core.service.restarted` carries `onchanges: [redis_hardening]` (not the previous `[redis_conf, redis_acl, redis_hardening, redis_tls_*]`). On at-create, a restart is not needed at all - the primary `core.service.running` raises the instance with the final config/ACL/cert.

### 3b. Privileged oneshot collector: root + narrow sandbox + Condition-gate

It happens that an auxiliary task requires **more** privileges than the main daemon - for example, the textfile hardware metrics collector reads block devices, IPMI or NVMe (only accessible by root). The straightforward "give root to the entire service" contradicts §3. The correct pattern (illustration - smartmon / nvme / ipmi collectors in [`node-exporter/`](../../examples/destiny/node-exporter/)): move the privileged work to a **separate oneshot `.service` + `.timer`**, and not hang it on the main daemon.

- **The main daemon remains unprivileged.** It only **reads** ready-made `.prom` from the textfile directory (`--collector.textfile`), the metrics themselves are written by oneshot collectors. Its unit directory is `ReadOnlyPaths=<textfile_dir>` (not RW): the daemon does not write there.
- **Oneshot - `Type=oneshot`, `User=root`, but the sandbox is as narrow as possible to suit the actual needs.** Root is not given "in general", but narrowed: `CapabilityBoundingSet=` leaves **only** the really necessary capabilities (smartmon - `CAP_SYS_RAWIO CAP_SYS_ADMIN` for RAW IO to disks), `DevicePolicy=closed` + dot `DeviceAllow=block-* r` opens **only** the required device class, `PrivateNetwork=yes` (the collector does not need a network), `ReadWritePaths=<textfile_dir>` is the only recording directory. The rest of the hardening block (`ProtectSystem=strict`, `ProtectKernel*`, `RestrictSUIDSGID`, `NoNewPrivileges`, etc.) is as in §3.
- **Writing `.prom` is atomic:** the collector writes to the temporary file and `mv` to the final file (`mktemp … > tmp; mv tmp <dir>/<name>.prom`) to prevent node_exporter from reading the half-written file.
- **Condition-gate against starting without hardware.** Each collector-unit (and its `.timer`) carries `Condition*`, which prevents it from starting where there is no hardware: smartmon - `ConditionVirtualization=no` (there are no physical disks on the VM), nvme - `ConditionPathExistsGlob=/dev/nvme[0-9]*`, ipmi - `ConditionPathExists=/dev/ipmi0`. The same `Condition*` is placed in both `.service` and its `.timer` (protection from both planned and manual `start`). Thanks to this, **installation** of the collector is also safe on the VM: destiny tasks deploy unit/timer, but systemd simply does not activate them without the corresponding device. This removes the need for per-host logic "to install a collector or not" inside destiny - gating is given to systemd.
- **Optionality of collectors - through the `input:`-list + `when:`.** Which collectors to install is selected by `input.collectors` (`type: array`, `items: { enum: [smartmon, nvme, ipmi] }`, default destiny - full set `[smartmon, nvme, ipmi]`); each collector task carries `when: "'<name>' in input.collectors"` (bare CEL, top-level expression key - without `${…}`). The two levels are independent: `when` decides whether **to install** collector artifacts at all; `Condition*` decides whether the supplied collector **starts** on this hardware. At the service level, this choice is forwarded by a parameter of the same name (in `monitoring` - `node_exporter_collectors`, `array<string>` with values `smartmon`/`nvme`/`ipmi`, default `[]`: there is no hardware on the VM → only the node_exporter kernel) and goes to destiny through `apply: input: { collectors: ${ input.node_exporter_collectors } }`.

## 4. TLS / web-config - via `extra_args`, do not sew

TLS and basic-auth of Prometheus family daemons are enabled with the `--web.config.file=<path>` flag. Pass this flag through `extra_args` (item 1), `web.yml` itself (certificates, password hashes) is **a separate task or a separate destiny**, not part of the daemon's destiny.

Reason: web-config is a secret-carrying artifact with its own life cycle (certificate rotation is independent of the daemon version). Sewing his render into the demon's destiny connects two independent cycles and drags secrets into someone else's area of ​​​​responsibility.

## 5. arch / os - from `soulprint.self`

The architecture (and any stable os fact of the target host) is read by destiny **directly** from `soulprint.self.os.arch`. After amendment 2026-06-18 ([ADR-009](../adr/0009-scenario-dsl.md) / [ADR-010](../adr/0010-templating.md), see [`docs/templating.md`](../templating.md)) the stable self-layer `soulprint.self.*` is also available in the CEL pass destiny - this is a per-host property, not a scenario-scope. The reference `node-exporter` does just that: in `input:` fields `arch` **no**, the tarball URL is collected directly from the fact:

```yaml
# tasks/install.yml node-exporter reference
url: "${ input.base_url + '/v' + input.version + '/node_exporter-' + input.version + '.linux-' + soulprint.self.os.arch + '.tar.gz' }"
```

- The destiny isolation boundary ([ADR-009](../adr/0009-scenario-dsl.md), §8) runs along **self vs run topology**: `soulprint.self.*` (stable fact of the current host) is available, and cross-host `soulprint.hosts`/`soulprint.where(...)` is scenario-only, cut off in destiny. One incarnation can mix amd64/arm64 hosts - each host gets its own `soulprint.self.os.arch`, so a separate `input: arch` is not needed (it would make the architecture the same for the entire run).
- `apply: input:` remains a channel for values ​​that destiny **not** can derive from its self-layer: caller-derived data, cross-host facts (`soulprint.where(...)`), `vault(...)`/`essence.*` - their scenario resolves them in itself and transmits them ready.
- soulprint already gives the value in release notation (`amd64`/`arm64`, see [`docs/soul/soulprint.md`](../soul/soulprint.md)) - mapping is not needed.

## 6. Idempotency of each step

Each destiny task is a no-op when applied again with the same inputs (destiny invariant, [concept.md](concept.md)). In practice, a promotional step has a clear "already done" sign:

- `core.url.fetched` / `core.file.rendered` - checksum/contents matched → no-op.
- `core.archive.extracted` — marker of the unpacked archive.
- `core.cmd.shell` / `core.exec.run` - `creates:` (result path already in place → do not execute).
- `core.pkg.installed` / `core.service.running` - declarative nature of the module (`present`/`running`).
- Restart - only reactively: `core.service.restarted` with `onchanges: [<register unit task>]`, and not unconditionally.

A step without the idempotency attribute (especially `cmd`/`exec` without `creates:`) is a bug of prod-grade destiny.

### 6a. Hot-reload is preferable to restart for services with live-reconfig

**Normative rule.** For a service that supports **on-the-fly reconfiguration** (live-reconfig - Redis, and similar), changes to **config / ACL / TLS material** in day-2 scenarios are applied **hot-reload** (without process restart), and **not** reactive restart. Restarting the daemon is expensive (resetting replicas/clients, unavailability window) and is not needed for such changes: the service can re-read them from a live instance.

Restart remains **only** for changes that hot-reload **does not physically cover** - primarily behind the **systemd-unit-level** (hardening drop-in: `ProtectSystem`/`ReadWritePaths`/`ExecStart`-override): they take effect only after `daemon-reload` + process restart, because this is the configuration of the process itself unit, not a service runtime directive (§3a).

This narrows the previous formulation of "restart reactively to a config/drop-in change": rendering the desired state to disk (`redis.conf`/`users.acl`/PEM) remains in destiny, but **revitalizing** the changes is done by the hot-reload-step of the day-2-script, rather than `core.service.restarted` by `onchanges` on these files.

Illustrations - day-2-scenarios of the service [`redis`](../../examples/service/redis/) (next to `restart`, which remains behind the unit level, §3a / §7a):

- [`update_config`](../../examples/service/redis/scenario/update_config/main.yml) — `community.redis.config` (CONFIG SET hot-settable + CONFIG REWRITE);
- [`add_user`](../../examples/service/redis/scenario/add_user/main.yml) — `community.redis.acl` (ACL LOAD rereads `aclfile`, without restart);
- [`rotate_tls`](../../examples/service/redis/scenario/rotate_tls/main.yml) - CONFIG SET `tls-*-file` (Redis 6.2+ rereads cert/key/CA live).

At the same time, the destiny rendering of these files itself remains idempotent according to §6 (the same ref/content → no-op), and the "already applied" attribute of the hot-reload step gives a comparison live ↔ the desired one in the plugin itself (honest diff `CONFIG GET` / `ACL LIST`), and not `onchanges` (see. [`docs/module/community/redis/README.md`](../module/community/redis/README.md)). The exception is an action operation like `rotate_tls` (force re-read SSL_CTX): it is non-idempotent **by design**, just like exec-style `reshard`.

## 7. Supply-chain

Any artifact downloaded from the network:

- **`checksum` is required and fail-closed (normative default).** In the `input:` contract, the hash field is `required: true` **without `default`**. No hash → honest refusal, not fetch with placeholder. `core.url.fetched` verifies the hash **before** the file is materialized - an incorrect hash is not sent to disk. This is done at the **service level**: the script [`monitoring`](../../examples/service/monitoring/) declares both `node_exporter_sha256` and `redis_exporter_sha256` as `required: true` without default and passes the value to destiny through `apply: input:`.
- **https-only.** Artifact URL is `https://` only. No `http://` for downloading binaries/archives.
- The hash is tied to the `(version, arch)` pair - taken from the official `sha256sums` release for a specific tarball.

**Documented mirror relaxation (optional `sha256`).** Reference [`node-exporter/`](../../examples/destiny/node-exporter/) declares `sha256` as `optional` with `default: ""` (`pattern: "^(sha256:[0-9a-f]{64})?$"`): defined → `core.url.fetched` verifies before publication; empty → download **without integrity check**, idempotency by SHA-256 content. This is a deliberate compromise under mirrors/nexus-proxy, where the hash may not be available in advance, and is **not** a relaxation of the rule above. The optionality of the hash in destiny itself is consistent with the fact that the fail-closed contract is kept **higher up the stack** - by the calling script (`monitoring` makes `node_exporter_sha256` mandatory). Conditions for the applicability of the relief:

- default-secure path - **set hash**; empty `sha256` increases supply-chain risk and is clearly marked with a warning in the `input:` description of destiny itself;
- relaxation is only allowed in conjunction with `base_url`-override on a trusted internal mirror; to download directly from public GitHub Releases, keep the hash required (as the service layer does);
- a script claiming a **strict** prod-grade contract (without clauses), declares the hash `required: true` without `default` in itself and forwards it to destiny.

## 7a. Day-2: source of truth = `incarnation.state`

**Normative rule for day-2 scenarios** (everything that is not `create`: `restart`, `update_config`, `add_user`, `rotate_tls`, `add_node`, `remove_node`, etc.). The Day-2 script reads the detailed fact about the service from **`incarnation.state`**, and **not** from `essence`/`input`.

Reason - `essence` and `input` for day-2 are **incomplete**:

- `create` accepts the operator's `input` (for example `input.tls`, `input.memory_mb`), **translates** it into an expanded configuration and writes it to `incarnation.state` (`state_changes`, [ADR-057](../adr/0057-state-changes-crud-verbs.md)). This `input` is only available at the time `create` - day-2-run does not see it.
- The operator on `create` can **override** the defaults of `essence` (in Soul Stack `input` beats `essence` - for example operator `input.tls.port`/`input.tls.ca_ref` over `essence.tls_*`). Day-2-script looking at `essence` will see **the author's underlay**, and not the actually deployed one - this leads to desynchronization "the script thinks one thing, but the host thinks another."

Therefore, the day-2 script takes the detailed fact (whether TLS is enabled, on what port the service is listening, what ACL users are set up, what is the topology of the shards) from `incarnation.state` - the only place where *what is actually applied* is recorded. `essence`/`input` on day-2 are used only for things that **fundamentally do not belong in `state` (for example, secrets - see below).

**Named intent field is preferable to parsing opaque config.** When `state` carries both opaque total (`redis_config`, computed by redis.conf-map) and **named intent field** (`state.tls`, `state.persistence`, `state.install`, ...), day-2 scenario reads the **named** field. It is typed, does not require bracket notation of hyphen keys, and explicitly expresses the intent of the statement (read-model), rather than being reverse-convolved from the write-model. So in the service `redis` (`state_schema v3`) next to `redis_config` live `tls`/`install`/`persistence`/`memory_mb`/`maxmemory_policy`/`modules`/`sysctl_settings` + topology (`shards`/`replicas`/`sentinel_quorum`); both views are filled with `create` from ONE compute pass (there is no out of sync), but day-2 reads namedfield.

Working illustration - `restart` service [`redis`](../../examples/service/redis/scenario/restart/main.yml): TLS discriminator for the plugin connection to Redis (via TLS or plaintext, on which port) is taken from the NAMED field of the expanded `incarnation.state.tls` (`state.tls.enable` / `state.tls.port`), not from `essence.tls_*` or parsing `redis_config['tls-port']`. If he had looked at `essence`, with the operator's TLS-override, day-2 would have gone with a plaintext connection to TLS-only Redis (health-gate failure; in the worst case, AUTH plaintext with an open plain-port).

Edge cases:

- **There are no secrets in `state`** (IS-invariant). PEM, passwords and other secret material from `state` are deliberately excluded. Their day-2 script resolves `vault(<ref>)` in the same way as `create` - `ref` is taken from the stable author-context (`essence.<...>_ref`) or path convention (`secret/redis/<incarnation>#password`). In `state` only the **path** to the secret on the host (for example `tls-ca-cert-file: /etc/redis/tls/ca.crt`) is materialized, not its contents; path - for rendering the config, not for passing it to the plugin (a plugin that reads a secret usually waits for **content**, not the path).
- **Reading a key with a hyphen** (`redis_config['tls-port']` and similar redis.conf directives) - bracket notation in **access** to the value, but checking for the presence of a key - with the `'<key>' in <map>` operator, **not** `has(<map>['<key>'])`: `has()` - CEL macro that accepts only field-selection (`has(x.y)`), index argument `has(x['y'])` is rejected by the parser ("invalid argument to has() macro"). A hyphen in the key name excludes field-access, so the protection against no-such-key is `has(incarnation.state) && has(incarnation.state.<col>) && '<key>' in incarnation.state.<col>` (two stages of `has()` cover the absence of `state` in a push/trial run without State and a not-yet-materialized collection). This rule is for cases when it is the opaque config with hyphen keys that is read; **named intent field** (`state.tls.enable`, unhyphenated) does not require this binding - field-selection and `default(...)` work directly, which is the reason to prefer namedfield (see above). So `redis/restart` after v3 discriminates against `default(incarnation.state.tls.enable, false)` instead of `'tls-port' in incarnation.state.redis_config`.

Checking the invariant - guard-case L0 on both paths of the discriminator: for `redis/restart` this is a pair of `rolling-restart-replicas` (state without `tls` → plaintext branch) and `rolling-restart-tls` (state with `tls.enable=true` → TLS branch, addr on `state.tls.port`, `tls_ca` = PEM from Vault).

## 8. Insulation destiny (ADR-009)

destiny sees **only its `input:`**. No reading someone else's context:

- No access to `incarnation.state`, to facts of other hosts, to `essence` service, to scenario-scope.
- Cross-host and cloud/vault data come exclusively through `apply: input:` from the caller (scenario resolves `soulprint.where(...)`, `vault(...)`, `essence.*` on its side and transfers the values).
- The result comes out only through the declared top-level `output:` ([output.md](output.md)), not through prying.

This is an invariant, not a recommendation: it keeps destiny reusable and independently testable.

## See also

- [concept.md](concept.md) - what is destiny, its invariants (atomicity/declarativity/idempotency/isolation).
- [tasks.md](tasks.md) - task format, `onchanges`/`register`/`creates`/`retry`.
- [input.md](input.md) - `input:`-contract (where `required`/`default`/`pattern` are validated).
- [output.md](output.md) - how destiny gives the result to the caller.
- [`docs/templating.md`](../templating.md) — CEL + Go text/template, `${…}` marker, `core.file.rendered`, non-string CEL cell rule.
- [`docs/soul/soulprint.md`](../soul/soulprint.md) - `soulprint.self.os.arch` and other host facts.
- [ADR-015](../adr/0015-core-modules-mvp.md) - a set of MVP core modules (`core.url`/`core.archive`/`core.cmd`/`core.file`/`core.service`/`core.user`/`core.group`).
- [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/) - convention standard: binary `node_exporter`, stateful account `node_exporter` (§2 stateful branch), version-aware install (`unless --version`, §6), systemd-hardening (§3), privileged textfile collectors smartmon/nvme/ipmi for systemd-sandbox (§3b), optional `sha256` for mirrors (§7 relaxation).

## Folder naming convention in `examples/`

The example folder in `examples/destiny/` and `examples/service/` is called **bare dependency name without parent prefix** (`destiny-`/`service-`): directory is `node-exporter/`, `redis/`, `monitoring/`, not `destiny-node-exporter/` `service-monitoring/`. The type (destiny or service) is determined by the parent directory; there is no need to duplicate it in the folder name. Salt plugin prefixes (`soul-mod-`/`soul-cloud-`/`soul-ssh-`) - **remain**: these are the names of plugin binaries, not example folders.
