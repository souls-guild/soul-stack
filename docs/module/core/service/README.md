# core.service

Management of OS services (activity + autostart). **Soul-side**, static
is built into the `soul` binary. Implementation - [`soul/internal/coremod/service/service.go`](../../../../soul/internal/coremod/service/service.go).

Backend is taken from the soulprint fact `os.init_system` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md));
**fallback** for an empty/unknown fact - runtime detection `util.DetectInitSystem`: **systemd**
(`systemctl --version`) → **openrc** (`rc-service --version`) → **sysv**
(`service --version`), in that order. systemd is checked first: `systemctl
--version` also works on minimal systems where systemd is installed, but not PID 1
(chroot/container) - the module goes to the systemd branch. If neither the fact nor the detection was given init -
step falls (`no supported init system detected (systemd/openrc/sysv)`).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `running` | The service is active. The optional param `enabled` (tri-state) controls autorun in one step. | `changed=true` if the service was inactive and started, OR if `enabled` was set and the autostart state had to be changed. Already active and (with `enabled`) autostart matches - `changed=false`. |
| `stopped` | The service has stopped. | `changed=true` if active and stopped. No longer active - `changed=false`. |
| `restarted` | Unconditional restart. | **Always `changed=true`** - the user explicitly asked for a restart (for example, after changing the config). There is no idempotency here intentionally. |
| `enabled` | Autostart when system boots (ortho to activity). | `changed=true` if autostart was disabled and enabled. Already enabled - `changed=false`. |
| `disabled` | Boot-autostart is off (mirror of `enabled`, ortho to activity - a disabled service may still be running). | `changed=true` if autostart was enabled and disabled. Already disabled - `changed=false`. |
| `masked` | The unit is masked (`systemctl mask`, symlink → `/dev/null`): start impossible manually or as a dependency; strictly stronger than `disabled`. **systemd-only**. | `changed=true` if the unit was not masked and got masked (or was enabled and got disabled first). Already masked - `changed=false`. openrc/sysv → error. |

Mutating states (`running` / `restarted` / `enabled`, **NOT** `stopped` / `disabled` /
`masked`) before its action on systemd-backend execute `systemctl daemon-reload` - controls behavior
optional param `daemon_reload` (see below).
reload itself **doesn't** affect the `changed` step.

## running — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. |
| `enabled` | bool | optional (tri-state) | Managing autostart **in one step** (Ansible parallel `service state=started enabled=yes`): **omitted** — do not touch autostart (we only control activity); **`true`** - additionally `enable`; **`false`** - additionally `disable`. enable/disable are idempotent (check via `is-enabled`). |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Controls `systemctl daemon-reload` before start (systemd). See § daemon_reload. |

## stopped — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. |

## restarted — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. (`enabled` for `restarted` does not apply - state only restarts.) |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Manages `systemctl daemon-reload` before restart (systemd). See § daemon_reload. |

## enabled — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Controls `systemctl daemon-reload` before enable (systemd). See § daemon_reload. |

## disabled — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. |

`disabled` is the mirror of `enabled`: it guarantees boot-autostart is **off**,
orthogonal to current activity (a disabled service may still be running - use
`stopped` to stop it). Idempotent via `is-enabled`; backend-agnostic
(systemd/openrc/sysv). `daemon_reload` does not apply (disable does not start the unit).

## masked — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Service/unit name. |

`masked` guarantees the unit is masked (`systemctl mask`, unit symlink → `/dev/null`):
the service cannot be started at all - neither manually nor pulled in as a dependency.
Strictly stronger than `disabled`.

- **systemd-only.** On openrc/sysv `masked` is **not** a no-op but an **error**
  (fail-closed): a silent no-op would give a false sense of protection (masking is a
  security control).
- **Disable-before-mask.** `masked` idempotently disables autostart **before** masking
  (systemd errors when masking a still-enabled unit). `changed` reflects the disable
  step and/or the mask step: already masked (and disabled) → `changed=false`.
- `daemon_reload` does not apply (masking does not start the unit).

## daemon_reload - rereading unit files

Optional param on states `running` / `restarted` / `enabled` (on `stopped`
**not** declared - reload is not needed there). Controls whether systemd rereads unit files
(`systemctl daemon-reload`) **before** mutating action.

**Why.** systemd keeps unit definitions in memory. After the unit file (or
drop-in) on the disk was changed - for example, `core.file.rendered` rendered a new one
`redis-server.service.d/hardening.conf` - the old definition is still relevant in memory,
`daemon-reload` has not yet been made. If you call `systemctl restart` at this point, systemd
**quietly restarts the service with the OLD unit definition** (the command runs with exit 0,
only warning `Unit file changed on disk, recommend reloading`) - unit file changes
do not apply, and the discrepancy "on disk is one thing, in memory is different" may not be noticed. To
you didn't have to manually put `core.exec.run systemctl daemon-reload` in front of each
`restarted` (see [conventions §3a](../../../destiny/production-conventions.md)),
`core.service` does the reload itself.

**Values** (`string`, closed-set; unknown value rejected on Validate, not silent):

| Meaning | Behavior (systemd-backend) |
|---|---|
| `auto` (**default**) | Gated by systemd flag: module reads `systemctl show <unit> --property=NeedDaemonReload --value`; if `yes` - does `systemctl daemon-reload`, otherwise nothing. Idempotent: reload only when the unit file is actually out of sync with the loaded definition. |
| `always` | `systemctl daemon-reload` unconditionally before action. |
| `never` | Explicit opt-out - reload is never done. |

**Edge cases:**

- **First install of a new unit file** → `NeedDaemonReload=no` → with `auto` reload
**not** executed (this is correct: systemd will pick up the definition that has not yet been loaded
on `start` itself). reload is needed precisely when *changing* an already loaded unit.
- **non-systemd init** (`openrc` / `sysv` / launchd) - daemon-reload **no-op** for any
param value: these init systems do not have an equivalent to `daemon-reload`.
- **reload does NOT affect the `changed` step.** `changed` remains a function only
start/restart/enable (for `restarted` - always `true`). reload is a side condition
applications, not independent change of service state. The fact that it was actually accomplished
reload is reflected **only** by the diagnostic output field `reloaded: true` (see
  [Output / register](#output--register)).

Existing tasks without `daemon_reload` receive `auto` - additive and backwards compatible.

## Capabilities / side-effects

- **Requires root** (`run_as_root`): start/stop/restart/enable/disable/mask via
init system (`mask` is systemd-only).
- **Executes subprocesses** (`exec_subprocess`): detect init
(`systemctl`/`rc-service`/`service` `--version`), status checks
  (`is-active`/`is-enabled`, `rc-service status`, `rc-update show`, `chkconfig
  --list`, `systemctl is-enabled` returning `masked`), daemon-reload
  (`systemctl show <unit> --property=NeedDaemonReload --value` with `auto`, then
  `systemctl daemon-reload`) and actions (`systemctl
start|stop|restart|enable|disable|mask`, `rc-service`/`rc-update`,
  `service`/`chkconfig`).
- **Changes the system:** service activity and/or its autostart.

## Output / register

`running` returns `{ name, active: true }` (+ `enabled: <bool>` if param
`enabled` was specified). `stopped` - `{ name, active: false }`. `restarted` —
`{ name, active: true }`. `enabled` — `{ name, enabled: true }`. `disabled` —
`{ name, enabled: false }`. `masked` — `{ name, masked: true }`.

If `daemon-reload` was actually executed at this step (see.
§ daemon_reload), is added to output
diagnostic field `reloaded: true`. The field appears **only** when actually
performed reload (on `stopped`, on non-systemd, with `never` and with `auto` without
`NeedDaemonReload` it is not) and is intended for diagnostics - at `changed` step it
has no effect.

## Examples

`running` + `enabled` one step, then reactive `restarted` through `onchanges`
to change the config/unit:

```yaml
- name: Ensure node_exporter is running and enabled at boot
  module: core.service.running
  params:
    name: node_exporter
    enabled: true

- name: Restart node_exporter because unit changed
  module: core.service.restarted
  onchanges: [node_exporter_unit]
  timeout: 30s
  params:
    name: node_exporter
```

(didactic summary of the link `running` + `onchanges`-`restarted`; a similar link for redis-server is in [`examples/destiny/redis/tasks/server.yml`](../../../../examples/destiny/redis/tasks/server.yml))

`onchanges:` accepts a **list** of registers - a restart is triggered if **at least one** of them has changed. So the daemon is restarted to both change the unit file and upgrade the binary:

```yaml
# node_exporter_unit — register unit render tasks;
# node_exporter_bin — register version-aware install tasks (core.cmd.shell with unless).
# Upgrading the version (install changed) will restart the service in the same way as editing a unit.
- name: Restart node_exporter because unit or binary changed
  module: core.service.restarted
  onchanges: [node_exporter_unit, node_exporter_bin]
  timeout: 30s
  params:
    name: node_exporter
```

(from [`examples/destiny/node-exporter/tasks/service.yml`](../../../../examples/destiny/node-exporter/tasks/service.yml) - the actual restart step of the standard combines both registers into one `onchanges` list)

In both examples `daemon_reload` is not set → `auto` is valid: if `onchanges` is a trigger
was a change to the unit file, systemd will see `NeedDaemonReload=yes` and the module will re-read it
unit before restart. Separate step `core.exec.run systemctl daemon-reload` before
`restarted` is no longer needed for this (see
[convention §3a](../../../destiny/production-conventions.md)). Unconditional reload -
`daemon_reload: always`; disable - `daemon_reload: never`:

```yaml
- name: Restart redis after drop-in change (force daemon-reload)
  module: core.service.restarted
  onchanges: [redis_hardening_dropin]
  params:
    name: redis-server
    daemon_reload: always
```

## Security

- **The module does not execute arbitrary code, but changing the state of the service is
real side-effect that can reduce host availability.** `core.service`
only calls init system commands
(`systemctl start|stop|restart|enable|disable` and OpenRC/sysv equivalents,
  [`svcAction`/`enable`/`disable`](../../../../soul/internal/coremod/service/service.go))
- it **doesn't** launch a shell and doesn't pass an arbitrary string. However, `stopped`
stops the service, and `restarted` **always** restarts (not intentionally
idempotent): on a critical service this is a manageable but real breakage
service. The name `name` must come from the author of Destiny/scenario - stop
wrong unit from untrusted `name` = denial of service.
- **What exactly starts is determined by the unit file, not the module.** `running`/`enabled`
launch and put into autorun a unit with the name `name`; **what is the code** for this
will be executed, specified by the service unit file on the host (its `ExecStart`, etc.).
Linking with [`core.file`](../file/README.md) is dangerous: if the autostart unit or
the binary under it is written in an untrusted `core.file` step, then `core.service.enabled`
pins its execution on every boot. Control the source of the unit file
exactly like the source of any executable artifact.
- **`enabled` / `disabled` / `masked` are orthogonal to activity.** `enabled` (state
and param) and `disabled` control boot-autostart and are idempotent (reconciliation
via `is-enabled`), but `enable` itself does not start a service and `disable` /
`disabled` do not stop an already running one - these are different axes. Do not
rely on `disabled` as a guarantee that the service is currently down (needs
`stopped`). `masked` blocks starting entirely (manually or as a dependency) and is
**systemd-only** - on openrc/sysv it is a loud error, never a silent no-op, so a
masked step cannot give a false sense of protection on an unsupported init.
- **Privileges.** Manifest
[`service.yaml`](../../../../shared/coremanifest/service.yaml) announces
  `required_capabilities: [run_as_root, exec_subprocess]` —
start/stop/restart/enable/disable/mask via init system always require root and
launch subprocesses (`systemctl`/`rc-service`/`service`/`rc-update`/`chkconfig`,
plus init detection and `is-active`/`is-enabled` checks). This is a **declaration** for
static reconciliation of `soul-lint` with `allowed_capabilities` host (see
  [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime elevation: the operation is executed with the privileges of the process
`soul` agent (as root), without elevation of rights inside the module.

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/file/README.md](../file/README.md) - `core.file.rendered` (typical source `onchanges:` for `restarted`).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs; **Amendment 2026-06-18** - centralized daemon-reload (`daemon_reload`); **Amendment 2026-07-17** - `disabled` / `masked` states (systemd-only mask, disable-before-mask).
- [destiny/production-conventions.md §3a](../../../destiny/production-conventions.md) - manual daemon-reload pattern (now covered by built-in `auto`).
- [ADR-018(b)](../../../adr/0018-soulprint-typed.md) — `SoulprintFacts.os.init_system` as **primary** backend source; runtime detection - fallback.
