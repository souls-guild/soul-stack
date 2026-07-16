# core.group

Managing local OS groups. **Soul-side**, statically built into
`soul`-binary. Implementation - [`soul/internal/coremod/group/group.go`](../../../../soul/internal/coremod/group/group.go).

Backend - `groupadd` / `groupdel`. Semantics of `present` - present-or-create:
existing group **will not be reconnected** (re-calling on an existing group is
`changed=false`, `gid` is not verified or corrected).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The group exists (created via `groupadd` if it does not exist). | `changed=true`, if there was no group and it was created. If the group already exists - `changed=false` (`gid` of the existing group is not checked). |
| `absent` | The group has been deleted. | `changed=true` if the group was deleted (`groupdel`). There is no group - `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Group name. |
| `gid` | int | optional | Explicit gid (`groupadd -g`). If not specified, gid selects `groupadd`. |
| `system` | bool | optional (default `false`) | System group (`groupadd -r`): gid from the system range. Compatible with `gid` (both can be specified). Needed for service accounts of stateful services (for example, primary group `redis`). |

Both optional params (`gid`/`system`) are applied **only during creation**. For
of an already existing group - no-op.

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | The name of the group to be deleted. |

## Capabilities / side-effects

- **Requires root** (`run_as_root`): `groupadd` / `groupdel` rule `/etc/group`.
- **Executes subprocesses** (`exec_subprocess`): `groupadd` (present) / `groupdel`
(absent). Existence check - in-process via `user.LookupGroup` (without
subprocess).
- **Changes the system:** set of local groups.

## Output / register

`present` returns `{ name, exists: true, created }`, where `created` = `true` if
the group was created by this step, and `false` if it already existed. `absent` —
`{ name, exists: false }`.

## Example

`core.group.present` creates a primary group for a stateful service account **to**
the user itself (`core.user -g` requires an existing group) - see
[hybrid rule §2](../../../destiny/production-conventions.md#2-service-account---hybrid-rule)
(for stateless daemons under `DynamicUser=yes` the group is not needed, systemd provides it).
Working example - group `node_exporter` in reference destiny
[`node-exporter`](../../../../examples/destiny/node-exporter/tasks/account.yml):

```yaml
- name: Ensure the node_exporter system group exists
  module: core.group.present
  params:
    name: node_exporter
    system: true
```

Link with `core.user` (the group is created BEFORE the user, because `core.user -g`
requires an existing primary group) - in the example [core/user/README.md](../user/README.md).

## Security

- **The main risk is the re-creation of a privileged group.** The module itself
only creates/deletes a group via `groupadd` / `groupdel` and **not** membership
manages (adds members to group [`core.user`](../user/README.md) via
`-G` / `-g`). But `name` is not validated for meaning: `core.group.present` with
`name: sudo` / `wheel` / `docker` will create a privileged group if it
no (for example on a fresh VM), and further `core.user.present` with
`groups: [<this group>]` will give the bearer the actual path to root. That is, the risk
not in `groupadd` itself, but in the fact that the group becomes a ready "carrier"
privileges" for future membership. Group name from `input.*` / `register.*`
/ `soulprint.*` must be trusted (by Destiny/scenario) and not external
input.
- **Privileges.** Manifest
[`group.yaml`](../../../../shared/coremanifest/group.yaml) announces
  `required_capabilities: [run_as_root, exec_subprocess]` — `groupadd` /
`groupdel` rules `/etc/group` and without UID 0 will not work, and both actions -
launching subprocesses. This is a **declaration** for static reconciliation of `soul-lint` with
`allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime elevation: the operation is executed with the privileges of the process
`soul` agent (under root), there is no elevation of rights inside the module - under the same root
both backends are used.
- **No validation of `gid`, no reconciliation.** `gid` is passed to `groupadd -g`
literally, without checking the range (verified by:
  [`group.go`](../../../../soul/internal/coremod/group/group.go) `applyPresent`
only converts params to flags; `Validate` only checks the type `gid`/
`system`). Idempotency present - present-or-create: for an existing one
group `gid` **not** checked and **not** corrected. Consequence: if the group
already has a name with a "foreign" gid, applyingPresent again will not align it -
the module trusts the first creator. Removal - via `absent` (`groupdel`);
active memberships in `/etc/passwd` the module does not clear - this is behavior
  `groupdel`.
- **Dangerous vs. correct.** Group name from untrusted source:

  ```yaml
  # DANGER: group name from external input - may be sudo/wheel/docker,
  # followed by core.user -G <this group> will give the path to root.
  - name: Ensure group
    module: core.group.present
    params:
      name: "${ input.group_name }"
  ```

For a service account, record the name of the system group in the Destiny author:

  ```yaml
  # SAFE: explicit unprivileged system group for the service account.
  - name: Ensure the app system group exists
    module: core.group.present
    params:
      name: appsvc
      system: true
  ```

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/user/README.md](../user/README.md) - `core.user` (primary group `-g` refers to it).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
