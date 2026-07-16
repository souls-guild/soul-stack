# core.user

Managing local OS users. **Soul-side**, statically built into
`soul`-binary. Implementation - [`soul/internal/coremod/user/user.go`](../../../../soul/internal/coremod/user/user.go).

Backend - `useradd` / `userdel` (busybox-compatible subset; on Alpine this is
package `shadow` or busybox-built-ins - both understand the flags used). Semantics
`present` - **present-or-create** (MVP): existing user **not
reconciled**, optional params are only valid when created. `usermod` in MVP
is not called.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The user exists (created via `useradd` if it does not exist). | `changed=true` if there was no user and it was created. If the user already exists - `changed=false` (reconcile uid/shell/home/groups/system/group **not executed**, see introductory paragraph). |
| `absent` | The user has been deleted. | `changed=true`, if the user has been deleted (`userdel`). There is no user - `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Username. |
| `uid` | int | optional | Explicit uid (`useradd -u`). If not specified, uid selects `useradd`. |
| `shell` | string | optional | Login shell (`useradd -s`). Empty/not specified → default `useradd`. |
| `home` | string | optional | Home directory (`useradd -d`). Created with the `-M` flag (without home auto-creation), as implemented in the code. Empty/not set → default `useradd`. |
| `groups` | []string | optional | Supplementary groups (`useradd -G a,b`). Groups must exist. |
| `system` | bool | optional (default `false`) | System account (`useradd -r`): uid from the system range, for service accounts of stateful services (for example `redis`). |
| `group` | string | optional | Primary group (`useradd -g`). The group **must already exist** - the caller creates it via `core.group` BEFORE. Different from `groups` (supplementary, `-G`). |

All optional params (`uid`/`shell`/`home`/`groups`/`system`/`group`)
are applied **only during creation**. For an existing user they
no-op - do not trigger `usermod`/reconcile.

## Input validation and params format

This is the **input-validation/safety of our code**, not the restriction of the :validation operator
cut off injections (leading `-` → argument confusion in argv `useradd`) and
deliberately broken input with an understandable error, **not** tightening the real restrictions
`useradd`. Triggered on `Validate` (early failure `soul-lint` / validation phase) and
again in `Apply` (if the step is called without a previous `Validate` phase - broken name
should not reach `useradd`/`userdel`).

| Param | Rule | Motivation |
|---|---|---|
| `name` | `NAME_REGEX` shadow-utils default: `^[a-z_][a-z0-9_-]*\$?$` (final `$` is the machine account Samba/NIS suffix literal), length ≤ 32, not empty. This is the convention of `useradd` itself (with `--badnames` he weakens it - we are in default). | The name must begin with a letter/`_` → leading `-` is excluded (arg-injection guard at the format level). Slash/space/special characters do not pass regex. |
| `uid` | an integer in the range `[0, 2147483647]` (uid_t is signed 32-bit on Linux). | Range-guard from known-bit input to subprocess launch; the type is already checking `OptIntParam`. |
| `shell` | if specified - **absolute path** (starting with `/`), without leading `-`. The existence of the file is **not** checked. | `useradd` does not require the existence of a shell (hybrid flexibility rule - like in Ansible); absoluteness + prohibition `-` cut off argument confusion. |
| `home` | if specified - **absolute path** (starting with `/`), without leading `-`. The directory is **not** created (flag `-M`). | Symmetrical `shell`: the path to home must be absolute, the injection form is cut off. |
| `group` (primary) | same `NAME_REGEX` as `name`. | Goes to `useradd -g <group>`: leading `-` otherwise it would be parsed as an option. |
| each of `groups` | same `NAME_REGEX` as `name`. | Goes to `useradd -G a,b`: each name is validated separately. |

**Arg-injection guard in argv.** The module puts `--` on top of the name format check
before positional `name`: `useradd … -- <name>` and `userdel -- <name>`. `useradd`
parses options via `getopt_long`, which understands `--` as the end of options (man
useradd) is defense-in-depth: even if the option-name would somehow pass the format-check,
it will not be interpreted as a flag. (`core.user` builds argv directly, without
`sh` - shell-injection is not relevant; it is the arg-confusion of the positional that is relevant
argument.)

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | The name of the user to be deleted. |

## Capabilities / side-effects

- **Requires root** (`run_as_root`): `useradd` / `userdel` rule `/etc/passwd`,
  `/etc/shadow`, `/etc/group`.
- **Executes subprocesses** (`exec_subprocess`): `useradd` (present) / `userdel`
(absent). Existence check - in-process via `user.Lookup` (without
subprocess).
- **Changes the system:** set of local users.

## Output / register

`present` returns `{ name, exists: true, created }`, where `created` = `true` if
the user was created by this step, and `false` if it already existed. `absent` —
`{ name, exists: false }`.

## Example

The choice "`core.user.present` vs `DynamicUser=yes`" is [hybrid rule of the food convention §2](../../../destiny/production-conventions.md#2-service-account---hybrid-rule): **stateless**-daemon without owned data-dir receives an ephemeral account from systemd (`DynamicUser=yes`, manual `core.user` is not needed), and **stateful**-service needs a stable one uid is the owner of the directory - it is started by `core.user.present`. A working example of a stateful account is `node_exporter` in the reference destiny [`node-exporter`](../../../../examples/destiny/node-exporter/tasks/account.yml) (the textfile directory of hardware metrics is experiencing restarts, a stable owner is needed); inline-redis_exporter in [`monitoring`](../../../../examples/service/monitoring/scenario/create/main.yml) - another pole: least-privilege `core.user.present` without stable state. Minimal template:

```yaml
# The primary group is created BEFORE the user (core.user -g requires an existing one).
- name: Ensure the node_exporter system group exists
  module: core.group.present
  params:
    name: node_exporter
    system: true

- name: Ensure the node_exporter system user exists
  module: core.user.present
  params:
    name: node_exporter
    system: true
    group: node_exporter
    shell: /usr/sbin/nologin
    home: /
```

(from [`examples/destiny/node-exporter/tasks/main.yml`](../../../../examples/destiny/node-exporter/tasks/main.yml))

## Security

- **Format/injections are checked, sense privilege is not.** Module
validates the **form** input ([`Validate`](../../../../soul/internal/coremod/user/user.go)
  + repeat in `applyPresent`): `name`/`group`/`groups` by `NAME_REGEX`, `uid` by
range, `shell`/`home` is an absolute path, and puts `--` before the positional one
name in argv (see Input Validation section). This closes **arg-injection** and
deliberately broken input. But the module **doesn't** evaluate how much the account being created
privileged: `uid: 0` creates a second user with root rights (uid 0 is
is the "root" for the kernel, the role name is not important - `0` passes the range check as
is a valid value), and `groups: ["sudo"]` / `["wheel"]` / `["docker"]` gives
the media is the actual path to root (group names are format-valid). These values ​​are
attack surface part: if they come from `input.*` / `register.*` /
`soulprint.*`, they should be trusted by the Destiny/scenario author, not external input.
Format-validation ≠ authorization: it catches the injection, not "dangerous, but valid"
meaning.
- **Privileges.** Manifest
[`user.yaml`](../../../../shared/coremanifest/user.yaml) announces
  `required_capabilities: [run_as_root, exec_subprocess]` — `useradd` / `userdel`
rules `/etc/passwd`, `/etc/shadow`, `/etc/group` and without UID 0 will not work, but
both actions are launching subprocesses. This is a **declaration** for static reconciliation
`soul-lint` from `allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime elevation: the operation is executed with the privileges of the process
`soul` agent (under root), there is no elevation of rights inside the module - under the same root
both `useradd` and `userdel` go.
- **We do not check the semantics of values - keep the input trusted.** Format - yes (see.
"Input Validation"), but the meaning is not. `shell` goes to `useradd -s` literally:
absoluteness is checked, **existence** of the login-shell file is not (this solves
`useradd` itself, or a valid but "non-working" shell remains - a hybrid rule
flexibility). `home` is transmitted through `-d` together with `-M` (directory **not**
is created by the module). `group` must exist in advance - the module does not create it
(it is created by [`core.group`](../group/README.md) in a separate step BEFORE; name format
the module checks whether the group is present in the system - not). Idempotency present -
present-or-create: already
existing user will **not** reconsider, so lower privileges
a previously created account can not be reapplyPresent **not** (uid/groups not
edits) - this reduces the risk of accidental edits, but also does not cure erroneously issued
privileges. Removal - via `absent` (`userdel`).
- **Dangerous vs. correct.** Privileged values ​​from an untrusted source:

  ```yaml
  # DANGER: uid/groups from external input creates a root-equivalent account.
  # input.uid = 0 → second root; input.extra_groups = ["sudo"] → path to root.
  - name: Create app user
    module: core.user.present
    params:
      name: appsvc
      uid: "${ input.uid }"
      groups: "${ input.extra_groups }"
  ```

For a service account, fix secure values in the Destiny author and not
put it in privileged groups:

  ```yaml
  # SAFE: system account without login and without sudo/wheel/docker.
  - name: Ensure the app system user exists
    module: core.user.present
    params:
      name: appsvc
      system: true
      group: appsvc
      shell: /usr/sbin/nologin
      home: /var/lib/appsvc
  ```

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/group/README.md](../group/README.md) - `core.group` (primary group `-g` is created by it).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
