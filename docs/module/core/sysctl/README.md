# core.sysctl

Managing kernel parameters (`vm.*`, `kernel.*`, `net.*`). **Soul-side**,
is statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/sysctl/sysctl.go`](../../../../soul/internal/coremod/sysctl/sysctl.go)
(state `present`) and
[`soul/internal/coremod/sysctl/applied.go`](../../../../soul/internal/coremod/sysctl/applied.go)
(state `applied`).

State `present` results in **both** sides being consistent: runtime value (via
`sysctl -w`) and a persist entry in `/etc/sysctl.d/<filename>.conf` (so that the value
experienced reboot). The current runtime value is read via `sysctl -n <name>`;
multi-value keys (tab-separated) are normalized before comparison so that
spaces vs tabs did not produce false diff.

State `applied` manages a SET of parameters through one deterministic drop-in
(see below): the module itself builds the content from the map (sorted keys), writes it atomically and
rereads pointwise `sysctl -p <file>` when changed.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The kernel parameter `name` has the value `value` (runtime) **and** the persist record `<name> = <value>` is in `/etc/sysctl.d/<filename>.conf`. | `changed=true` if the runtime value was different (`sysctl -w` was applied) **or** the persist file was missing/contained a different line (overwritten). Both matched - `changed=false`. |
| `applied` | The BULK set `settings` (map) is materialized as ONE drop-in `/etc/sysctl.d/<filename>.conf` (sorted keys, format `key = value`); reload via `sysctl -p <file>` (specifically via drop-in, NOT the entire `--system`). | `changed=true`, if the drop-in content was different from the existing one (or there was no file) → atomic overwrite. Matched - `changed=false`. **reload itself `changed` does NOT mark**; gating: `never` - never, `always` - certainly, `auto` (default) - only with file-change. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Kernel parameter name (`vm.overcommit_memory`, `net.ipv4.ip_forward`). |
| `value` | string | required | Target value. Comparison with the current one - after normalization by fields (multi-value keys are reduced to one space + trim). |
| `filename` | string | optional (default — `<name>` with `.` replaced by `-`) | The name of the persist file is `/etc/sysctl.d/`. If it does not end with `.conf`, the suffix is ​​added automatically. By default `vm.overcommit_memory` → `vm-overcommit_memory.conf`. |

## applied — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `settings` | map `string→string` | required | Set kernel parameter→value. Keys are **sorted** when rendering drop-in → content is deterministic between runs (no false `changed`/re-reload). Values ​​are strings (multi-value parameters separated by spaces). |
| `filename` | string | required | Drop-in name in `/etc/sysctl.d/` (eg `30-redis`). If it does not end with `.conf`, the suffix is ​​added automatically. `filepath.Join(/etc/sysctl.d, …)` holds an entry inside a directory. |
| `reload` | string enum `auto`/`always`/`never` | optional (default `auto`) | When to do `sysctl -p <file>` (point by drop-in): `auto` - only when changing the file; `always` - certainly; `never` - opt-out (do not do it at all). reload itself does NOT mark `changed`. Same dictionary as `daemon_reload` in `core.service`. |
| `ignore_failures` | bool | optional (default `false`) | `true` → reload via `sysctl -e -p <file>` (`-e`/`--ignore` suppresses read-only/non-existent keys in containers without wasting the run). Explicit opt-in. |

## Capabilities / side-effects

- **Requires root** (`run_as_root`): `sysctl -w`/`sysctl -p` and entry to `/etc/sysctl.d/`.
- **Executes subprocesses** (`exec_subprocess`): `sysctl -n <name>` (read
current value, `present`), `sysctl -w <name>=<value>` (runtime application,
  `present`), `sysctl [-e] -p <file>` (reload drop-in, `applied`).
- **Changes the system:** runtime kernel parameter(s) and persist config(s) in
  `/etc/sysctl.d/`.
- **Writes outside `/var/lib/soul-stack`** (`fs_write_root`): creates a directory
`/etc/sysctl.d` if necessary, writes persist-file/drop-in. `present` —
mode `0644`; `applied` - atomic write with preserve-by-default mode
existing file (the new one is `0644`). Write only occurs when diff
(the idempotent no-op file does not touch).

## Output / register

- `present` returns `{ name, value, path }`, where `path` is the full path to
persist file (`/etc/sysctl.d/<filename>.conf`).
- `applied` returns `{ path, settings }`, where `path` is the full path to the drop-in,
`settings` — number of keys applied.

## Examples

```yaml
# present - one key.
- name: Enable IPv4 forwarding persistently
  module: core.sysctl.present
  params:
    name: net.ipv4.ip_forward
    value: "1"
```

```yaml
# applied - bulk set with one drop-in + point reload. ignore_failures for
# containers, where the vm.* part is read-only.
- name: Apply redis sysctl kernel parameters
  module: core.sysctl.applied
  params:
    filename: 30-redis
    reload: auto
    ignore_failures: true
    settings:
      vm.overcommit_memory: "1"
      vm.swappiness:        "1"
      net.core.somaxconn:   "65535"
```

(`applied` is used in the host-tuning extras of the example `examples/destiny/redis`)

## Security

- **The main risk is changing the kernel parameter as root: incorrect value = DoS
or weakening the kernel protection.** The module applies `value` directly via
`sysctl -w <name>=<value>` and writes a persist entry to `/etc/sysctl.d/`
  ([`ensureRuntime`/`ensurePersist`](../../../../soul/internal/coremod/sysctl/sysctl.go))
is a global kernel setting that affects the entire host. Wrong value
hits availability (for example `vm.max_map_count` or `fs.file-max` is too
few → processes crash at limits; `net.*`-parameters break the network) or by
security (for example, weakening `kernel.*`/`net.*` protections). `value` from
`input.*` / `register.*` / `soulprint.*` must be trusted (author
Destiny/scenario) rather than external input.
- **There is NO validation of the value and name - this is by design MVP.** Verified by:
  [`sysctl.go`](../../../../soul/internal/coremod/sysctl/sysctl.go) `Apply`
only reads `name`/`value`/`filename` as strings and passes `value` to
`sysctl -w` literally; `Validate` delegated to manifest
([`sysctl.yaml`](../../../../shared/coremanifest/sysctl.yaml)) and checks
only known-state + required (`name`/`value`), but **not** range, type or
the validity of the parameter itself. Unknown/non-numeric `value` catches itself
`sysctl -w` (non-zero exit → step falls), and not the module - the module does not "underlie"
straws" in advance. The file name `filename` is normalized (`.`→`-`, suffix
`.conf`), but the path is always inside `/etc/sysctl.d/` (`filepath.Join(m.Dir, …)`) -
You cannot write a persist file outside the directory via `filename`.
- **Privileges.** Manifest
[`sysctl.yaml`](../../../../shared/coremanifest/sysctl.yaml) announces
  `required_capabilities: [run_as_root, exec_subprocess, fs_write_root]` —
changing the kernel parameter and writing to `/etc/sysctl.d/` requires UID 0,
the application goes through the subprocess `sysctl` (`-n`/`-w`), and the persist file is written
outside `/var/lib/soul-stack`. This is a **declaration** for static reconciliation
`soul-lint` from `allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
but **not** runtime escalation: backend calls and persist file writing come with
privileges of the `soul`-agent process (root), there is no elevation of rights inside the module.
This is the most "system-global" of the zone modules: the effect is on the entire host core, and
more than one account/catalogue.
- **Persistence increases the cost of error.** Each step affects **both** parties:
runtime (`sysctl -w`, effective immediately) **and** persist
(`/etc/sysctl.d/<filename>.conf`, mode `0644`, undergoing reboot). Therefore
an incorrect value will not "fix itself" after a reboot - it will remain in
config and will be applied again. Rollback - separate `core.sysctl.present` with correct
value (the module will overwrite both the runtime and persist files); removal
there is no persist record as a separate state in MVP.
- **Dangerous vs. correct.** Parameter value from an untrusted source:

  ```yaml
  # DANGER: value from external input is applied to the kernel without checking.
  # input.somaxconn = "0" or garbage → network degradation / sysctl -w crash.
  - name: Tune net backlog
    module: core.sysctl.present
    params:
      name: net.core.somaxconn
      value: "${ input.somaxconn }"
  ```

Commit the checked value to the Destiny author:

  ```yaml
  # SAFE: Explicit checked value set by the Destiny author.
  - name: Enable IPv4 forwarding persistently
    module: core.sysctl.present
    params:
      name: net.ipv4.ip_forward
      value: "1"
  ```

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
